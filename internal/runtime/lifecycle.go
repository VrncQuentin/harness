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

// Stop tears down runtime-owned services that need explicit shutdown.
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
// Unlike a reload, a drain timeout here does not abort anything — there is
// no "current generation" left to preserve for a later retry, since the
// process is exiting regardless. It is logged and Stop proceeds, which is
// the same "best effort, do not hang shutdown forever" reasoning already
// applied to task cancellation and the session flush below.
//
// Live sessions are flushed after the drain (not before, and not
// interleaved with it) so the summarizer can still reach the live
// llama-server and so no session-modifying request can still be admitted
// once the flush runs — see flushSessionsForReload's doc comment for why
// that ordering matters; the same reasoning applies here. The flush has its
// own context with a 10s timeout - if llama-server is unhealthy, the
// summarizer call will error and the flush will return an error, which we
// log and continue rather than block shutdown indefinitely. The .json
// sidecars survive in the working tree for next-session resume even if the
// summary commit never lands.
func (rt *Runtime) Stop(uiServer *ui.Server) {
	rt.mu.Lock()
	q := rt.reqQueue
	apiSrv := rt.apiServer
	tasks := rt.taskRunner
	rt.mu.Unlock()

	if tasks != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := tasks.CancelAll(ctx); err != nil {
			slog.Warn("task loop shutdown wait", "err", err)
		}
		cancel()
	}
	if uiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := uiServer.DrainGenerationRequests(ctx); err != nil {
			slog.Warn("UI request drain on shutdown", "err", err)
		}
		cancel()
	}
	if apiSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := apiSrv.Shutdown(shutCtx); err != nil {
			slog.Warn("api server shutdown", "err", err)
		}
		cancel()
	}
	if mgr := rt.SessionManager(); mgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := mgr.FlushAll(ctx); err != nil {
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
