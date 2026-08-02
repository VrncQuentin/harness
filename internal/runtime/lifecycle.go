package runtime

import (
	"context"
	"fmt"
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
	uiServer.SetSnapshotProvider(rt)
	rt.refreshProjectDirectoryWarnings(uiServer)
	rt.startServices(ctx, uiServer, events, metricsStore)
	rt.startMemoryAndAPI(ctx, uiServer, metricsStore, &rt.cfg)
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
	return rt.newInferenceClientForPort(model.Port)
}

// newInferenceClientForPort builds an inference client for a concrete port.
// The port always comes from the running model, never the preferred one:
// under llama_on_switch=keep the harness keeps talking to wherever llama-server
// actually runs.
func (rt *Runtime) newInferenceClientForPort(port int) inference.Client {
	return inference.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", port),
		httpclient.NewStreaming(),
	)
}

func (rt *Runtime) ensureInferenceClient() inference.Client {
	if rt.inferClient == nil {
		rt.inferClient = rt.newInferenceClient()
	}
	return rt.inferClient
}

func (rt *Runtime) newEmbedderClientFor(cfg *config.Config) embedder.Client {
	return embedder.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", cfg.Embedder.Port),
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

// Stop tears down runtime-owned services that need explicit shutdown. It is
// retained for tests and compatibility; production shutdown goes through
// Shutdown, which cancels the root context and owns the whole lifecycle.
//
// Stop performs one shutdown attempt without a root-context cancel (callers
// own their service contexts and process managers), with each bounded wait
// capped at defaultDrainTimeout. A second call is a no-op because the first
// released ownership of everything proven idle.
func (rt *Runtime) Stop() {
	rt.Shutdown(nil, defaultDrainTimeout)
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

	embedCfg := cfg.Embedder
	rt.embedMgr = proc.NewManager(proc.ManagerConfig{
		Name: "embedder",
		BuildArgs: func() (string, []string) {
			return embedderArgsForConfig(embedCfg)
		},
		HealthURL:   embedderHealthURL(embedCfg),
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
