package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/embedder"
	"github.com/VrncQuentin/harness/internal/httpclient"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/ui"
)

// Start brings llama-server, embedder, queue, memory, prompt, and API services
// up under the current config.
func (rt *Runtime) Start(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.refreshProjectDirectoryWarnings(uiServer)
	rt.startServices(ctx, uiServer, events, metricsStore)
	rt.startMemoryAndAPI(ctx, uiServer, metricsStore)
}

// Managers returns the process managers currently owned by the runtime.
func (rt *Runtime) Managers() (*proc.Manager, *proc.Manager) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.llamaMgr, rt.embedMgr
}

// QueueStats returns the live request queue depth and capacity. A zero capacity
// means the queue has not been configured yet.
func (rt *Runtime) QueueStats() (int, int) {
	rt.mu.Lock()
	q := rt.reqQueue
	rt.mu.Unlock()
	if q == nil {
		return 0, 0
	}
	return q.Depth(), q.MaxDepth()
}

func (rt *Runtime) newInferenceClient() inference.Client {
	return rt.newInferenceClientForModel(rt.effectiveModelFor(&rt.cfg))
}

// newInferenceClientForModel builds a client targeting model's port directly,
// for a caller that already has the model config it wants a client for and
// must not read rt.cfg to get it — notably ApplyConfig's model-endpoint-change
// path, which computes newModel before rt.cfg is committed to the loaded
// config (see config.go) and would otherwise build a client pointed at the
// old port by reading rt.cfg back out of newInferenceClient.
func (rt *Runtime) newInferenceClientForModel(model config.ModelConfig) inference.Client {
	return inference.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", model.Port),
		httpclient.NewStreaming(),
	)
}

func (rt *Runtime) ensureInferenceClient() inference.Client {
	if rt.inferClient == nil {
		rt.inferClient = rt.newInferenceClient()
	}
	return rt.inferClient
}

func (rt *Runtime) newEmbedderClient() embedder.Client {
	return embedder.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", rt.cfg.Embedder.Port),
		httpclient.NewStreaming(),
	)
}

// RestartLlama restarts the current llama-server manager when one exists.
func (rt *Runtime) RestartLlama() {
	if m, _ := rt.Managers(); m != nil {
		m.Restart()
	}
}

// RestartEmbedder restarts the current embedder manager when one exists.
func (rt *Runtime) RestartEmbedder() {
	if _, m := rt.Managers(); m != nil {
		m.Restart()
	}
}

// Stop tears down runtime-owned services that need explicit shutdown. ctx
// bounds every phase's own timeout the same way it does in
// quiesceMemoryAndAPI/shutdownAPIServerForReload — each phase derives its
// timeout from ctx via context.WithTimeout, so a ctx with an earlier
// deadline than a given phase's own tightens that phase's wait instead of
// extending it. Ordinary shutdown passes context.Background().
//
// uiServer is drained the same way a reload drains it — closing generation
// admission and waiting for in-flight requests — before anything below is
// closed. This used to be entirely absent: Stop took no *ui.Server and
// never touched its generation gate at all, so the elaborate leasing this
// migration built for reloads simply did not apply to normal shutdown.
// main.go's onQuit calls Stop before cancelling the context that eventually
// tears down the UI listener (rootCancel), so without this, a chat or
// memory request could start — or still be running — while FlushAll
// snapshots sessions below and owned.close() closes the handles it reads
// through: the same use-after-close hazard the reload path was built to
// close, just on the shutdown path instead.
//
// A drain (or API shutdown) timeout means a request is still actually
// running against the current generation — not merely that admission of
// new ones failed to close in time. Closing handles regardless would not
// just risk a *new* request landing in the gap (the reopen-on-timeout
// mistake DrainGenerationRequestsForShutdown itself already closes, see its
// own doc comment); it would pull the handles out from under the one
// request the drain was waiting for and could not. There is no generation
// left to preserve for a later retry the way an aborted reload preserves
// one, since the process is exiting regardless, so Stop cannot "abort" the
// way ApplyConfig does either. What it can do is stop short of the unsafe
// steps: on a timeout from either the UI drain or the API shutdown, Stop
// skips FlushAll and the final owned.close() and returns, leaving those
// handles open for the OS to reclaim on process exit — safe, since nothing
// past this point in Stop depends on them being closed, only on them not
// being closed too early. The request queue is still stopped either way:
// Queue.Stop closes intake and waits for the worker to drain requests it
// already accepted, touching neither rt.memHandles nor the API/UI
// generation gate, so it carries none of this risk.
//
// Live sessions are flushed after the drain (not before, and not
// interleaved with it) so the summarizer can still reach the live
// llama-server and so no session-modifying request can still be admitted
// once the flush runs — see flushSessionsForReload's doc comment for why
// that ordering matters; the same reasoning applies here. The flush has its
// own context with a 10s timeout - if llama-server is unhealthy, the
// summarizer call will error and the flush will return an error, which we
// log and continue rather than block shutdown indefinitely (this is a
// separate, already-tolerated failure mode from the quiesce timeout above:
// the flush itself still runs under the same handles, just may not finish
// summarizing everything). The .json sidecars survive in the working tree
// for next-session resume even if the summary commit never lands.
//
// Stop takes applyMu for its entire call and sets stopping under it before
// doing anything else. ApplyConfig checks stopping right after acquiring
// applyMu (see config.go) and bails out if it is set. Without this, /config
// and /retry — which intentionally sit outside the generation gate, since
// an apply drains that gate itself — could start, or be mid-flight, while
// Stop is shutting down: an apply completing concurrently would rebuild
// services and reopen admission that Stop is in the middle of closing, and
// Stop could then go on to close the replacement generation's handles out
// from under a runtime that thinks it just finished reloading.
func (rt *Runtime) Stop(ctx context.Context, uiServer *ui.Server) {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()
	rt.mu.Lock()
	rt.stopping = true
	q := rt.reqQueue
	apiSrv := rt.apiServer
	tasks := rt.taskRunner
	rt.mu.Unlock()

	if tasks != nil {
		cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := tasks.CancelAll(cancelCtx); err != nil {
			slog.Warn("task loop shutdown wait", "err", err)
		}
		cancel()
	}

	quiesced := true
	if uiServer != nil {
		drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := uiServer.DrainGenerationRequestsForShutdown(drainCtx); err != nil {
			slog.Warn("UI request drain on shutdown", "err", err)
			quiesced = false
		}
		cancel()
	}
	if apiSrv != nil {
		shutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := apiSrv.Shutdown(shutCtx); err != nil {
			slog.Warn("api server shutdown", "err", err)
			quiesced = false
		}
		cancel()
	}

	if !quiesced {
		slog.Warn("shutdown proceeding without a clean quiesce: a request is still running against the current generation, so the session flush and memory/git handle close are skipped rather than risk closing them underneath it — the process exiting will reclaim them instead")
		if q != nil {
			q.Stop()
		}
		return
	}

	if mgr := rt.SessionManager(); mgr != nil {
		flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := mgr.FlushAll(flushCtx); err != nil {
			slog.Warn("session flush on shutdown", "err", err)
		}
		cancel()
	}
	if q != nil {
		q.Stop()
	}

	// The pinned memory-repo and index handles go last: the session flush above
	// writes through them.
	rt.mu.Lock()
	owned := rt.memHandles
	rt.memHandles = memoryHandles{}
	rt.mu.Unlock()
	owned.close()
}

// WaitManagers waits for process manager goroutines to exit after their context
// has been cancelled. It is safe to call when either manager is nil.
func (rt *Runtime) WaitManagers(ctx context.Context) {
	llama, embed := rt.Managers()
	if llama != nil {
		if err := llama.Wait(ctx); err != nil {
			slog.Warn("llama manager shutdown wait", "err", err)
		}
	}
	if embed != nil {
		if err := embed.Wait(ctx); err != nil {
			slog.Warn("embedder manager shutdown wait", "err", err)
		}
	}
}

// startServices brings llama-server, embedder, queue, and metrics up under the
// current rt.cfg. Caller must hold rt.mu.
func (rt *Runtime) startServices(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) {
	cfg := &rt.cfg
	model := rt.effectiveModelFor(cfg)

	rt.llamaMgr = proc.NewManager(proc.ManagerConfig{
		Name:        "llama-server",
		BuildArgs:   func() (string, []string) { return llamaArgsForModel(model) },
		HealthURL:   llamaHealthURL(model),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.logRings.Llama,
	})
	go rt.llamaMgr.Run(ctx)

	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name: "embedder",
		BuildArgs: func() (string, []string) {
			return embedderArgsForConfig(cfg.Embedder)
		},
		HealthURL:   embedderHealthURL(cfg.Embedder),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.logRings.Embed,
	})
	go rt.embedMgr.Run(ctx)

	inferClient := rt.newInferenceClient()
	rt.inferClient = inferClient
	rt.reqQueue = queue.New(cfg.Queue.MaxDepth, inferClient)
	if metricsStore != nil {
		rt.reqQueue.SetMetrics(metrics.NewRecorder(metricsStore))
	}
	if err := rt.reqQueue.Start(ctx); err != nil {
		uiServer.AddStartupError(fmt.Errorf("queue start: %w", err))
	}

	if metricsStore != nil {
		go recordMetrics(ctx, metricsStore, rt.llamaMgr, rt.embedMgr, rt.reqQueue, func() int {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			return rt.cfg.Metrics.RetentionDays
		})
	}

	rt.started = true
}
