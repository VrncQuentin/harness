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
	return rt.newInferenceClientForModel(model)
}

// newInferenceClientForModel builds an inference client for the resolved
// active model. A local endpoint talks to the llama-server it spawns on the
// model port; an external endpoint talks to its base URL with the selected
// model id and optional API key.
func (rt *Runtime) newInferenceClientForModel(model config.ModelConfig) inference.Client {
	if model.Kind == config.EndpointKindOpenAI {
		return inference.NewClientForBackend(model.BaseURL, model.APIKey, model.ModelID, httpclient.NewStreaming())
	}
	return rt.newInferenceClientForPort(model.Port)
}

// newInferenceClientForPort builds an inference client for a concrete local
// port. The port always comes from the running model, never the preferred one:
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

	// A local endpoint owns the llama-server process the harness spawns. An
	// external endpoint has no process: the harness talks to the endpoint's
	// base URL directly, so no llama-server manager is created.
	rt.llamaMgr = nil
	if model.Kind == config.EndpointKindLocal {
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
	} else {
		slog.Info("external model backend active; not spawning llama-server",
			"endpoint", model.EndpointID, "model", model.ModelID, "base_url", model.BaseURL)
	}

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
