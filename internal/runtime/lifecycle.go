package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

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
	model := rt.effectiveModelFor(&rt.cfg)
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
// Live sessions are flushed first so the summarizer can still reach
// the live llama-server. The flush has its own context with a 10s
// timeout - if llama-server is unhealthy, the summarizer call will
// error and the flush will return an error, which we log and continue
// rather than block shutdown indefinitely. The .json sidecars survive
// in the working tree for next-session resume even if the summary
// commit never lands.
func (rt *Runtime) Stop() {
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
	if mgr := rt.SessionManager(); mgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := mgr.FlushAll(ctx); err != nil {
			slog.Warn("session flush on shutdown", "err", err)
		}
		cancel()
	}
	if apiSrv != nil {
		apiSrv.Stop()
	}
	if q != nil {
		q.Stop()
	}
	rt.mu.Lock()
	if dr, ok := rt.globalMem.(io.Closer); ok {
		_ = dr.Close()
	}
	if rt.activeMem != nil && rt.activeMem != rt.globalMem {
		if dr, ok := rt.activeMem.(io.Closer); ok {
			_ = dr.Close()
		}
	}
	rt.mu.Unlock()
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
