package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/queue"
	"github.com/vrnc/harness/internal/ui"
	"github.com/vrnc/harness/pkg/httpclient"
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
	hr := rt.hotReload
	rt.mu.Unlock()

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
	if hr != nil {
		if err := hr.Close(); err != nil {
			slog.Warn("prompt hot-reload close", "err", err)
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

	rt.llamaMgr = proc.NewManager(proc.ManagerConfig{
		Name: "llama-server",
		BuildArgs: func() (string, []string) {
			return proc.LlamaArgs(
				cfg.Model.Binary,
				cfg.Model.ModelPath,
				cfg.Model.CtxSize,
				cfg.Model.GPULayers,
				cfg.Model.NParallel,
				cfg.Model.Port,
				cfg.Model.Verbose,
				cfg.Model.CacheTypeK,
				cfg.Model.CacheTypeV,
			)
		},
		HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Model.Port),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.logRings.Llama,
	})
	go rt.llamaMgr.Run(ctx)

	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name: "embedder",
		BuildArgs: func() (string, []string) {
			return proc.EmbedderArgs(
				cfg.Embedder.Binary,
				cfg.Embedder.ModelPath,
				cfg.Embedder.Port,
				cfg.Embedder.Verbose,
			)
		},
		HealthURL:   fmt.Sprintf("http://127.0.0.1:%d/health", cfg.Embedder.Port),
		Events:      events,
		CheckPeriod: 5 * time.Second,
		HTTPClient:  httpclient.New(),
		Output:      rt.logRings.Embed,
	})
	go rt.embedMgr.Run(ctx)

	inferClient := inference.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", cfg.Model.Port),
		httpclient.NewStreaming(),
	)
	rt.reqQueue = queue.New(cfg.Queue.MaxDepth, cfg.Queue.WALPath, inferClient)
	if err := rt.reqQueue.Start(ctx); err != nil {
		uiServer.AddStartupError(fmt.Errorf("queue WAL error: %w", err))
	}

	if metricsStore != nil {
		go recordMetrics(ctx, metricsStore, rt.llamaMgr, rt.embedMgr, rt.reqQueue)
	}

	rt.started = true
}
