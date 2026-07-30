package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/ui"
)

// ApplyConfig reloads config from the store, validates it, and either starts
// services for the first time or reconfigures the live ones to match. Tier-3
// changes (UI port, queue) are returned as RestartNeeded so the UI can flag
// them - no live apply path exists for those yet.
func (rt *Runtime) ApplyConfig(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
) ui.ApplyResult {
	uiServer.ClearStartupErrors()
	uiServer.SetProjectDirectoryWarnings("", nil)
	if rt.cfgStore == nil {
		uiServer.AddStartupError(ErrConfigStoreUnavailable)
		return ui.ApplyResult{}
	}
	loaded, wasSaved, lerr := rt.cfgStore.Load()
	if lerr != nil {
		uiServer.AddStartupError(fmt.Errorf("config load: %w", lerr))
		return ui.ApplyResult{}
	}
	uiServer.SetFirstRun(!wasSaved)
	if !wasSaved {
		return ui.ApplyResult{}
	}
	if verr := config.Validate(loaded); verr != nil {
		uiServer.AddStartupError(verr)
		return ui.ApplyResult{}
	}
	if !ValidatePaths(uiServer, loaded) {
		return ui.ApplyResult{}
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()

	old := rt.cfg
	oldModel := rt.effectiveModelFor(&old)
	newModel := rt.effectiveModelFor(loaded)
	modelEndpointChanged := oldModel.Port != newModel.Port
	embedderEndpointChanged := old.Embedder.Port != loaded.Embedder.Port
	rt.cfg = *loaded
	rt.refreshProjectDirectoryWarnings(uiServer)

	var result ui.ApplyResult

	if !rt.started {
		slog.Info("starting services", "model_port", newModel.Port, "embed_port", loaded.Embedder.Port)
		rt.startServices(ctx, uiServer, events, metricsStore)
		rt.startMemoryAndAPI(ctx, uiServer, metricsStore)
		result.LiveApplied = true
	} else {
		needsMemoryAPIRetry := rt.memoryAPIUnavailable()
		if oldModel != newModel {
			slog.Info("reconfiguring llama-server", "old_port", oldModel.Port, "new_port", newModel.Port)
			rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(newModel) }, llamaHealthURL(newModel))
			result.LiveApplied = true
		}
		if old.Embedder != loaded.Embedder {
			slog.Info("reconfiguring embedder", "old_port", old.Embedder.Port, "new_port", loaded.Embedder.Port)
			rt.embedMgr.Reconfigure(func() (string, []string) {
				return embedderArgsForConfig(loaded.Embedder)
			}, embedderHealthURL(loaded.Embedder))
			result.LiveApplied = true
		}
		if modelEndpointChanged && rt.reqQueue != nil {
			client := rt.newInferenceClient()
			rt.inferClient = client
			rt.reqQueue.SetClient(client)
		}

		if old.Prompt != loaded.Prompt ||
			old.API != loaded.API ||
			old.Loop != loaded.Loop ||
			old.Agent.Active != loaded.Agent.Active ||
			old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug ||
			modelEndpointChanged ||
			embedderEndpointChanged ||
			needsMemoryAPIRetry {
			rt.quiesceMemoryAndAPI(ctx)
			if old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug {
				rt.handleProjectSwitch(ctx, uiServer, &old, loaded)
			}
			slog.Info("rebuilding memory and api services")
			if rt.startMemoryAndAPI(ctx, uiServer, metricsStore) {
				result.LiveApplied = true
			}
		}
	}

	if old.UI.Port != loaded.UI.Port {
		result.RestartNeeded = append(result.RestartNeeded, "UI port")
	}
	if old.Queue.MaxDepth != loaded.Queue.MaxDepth {
		result.RestartNeeded = append(result.RestartNeeded, "queue max depth")
	}
	if old.Log.RingMaxEntries != loaded.Log.RingMaxEntries && rt.logRings.Log != nil {
		rt.logRings.Log.Resize(loaded.Log.RingMaxEntries)
		result.LiveApplied = true
	}
	if old.Log.ProcMaxLines != loaded.Log.ProcMaxLines {
		if rt.logRings.Llama != nil {
			rt.logRings.Llama.Resize(loaded.Log.ProcMaxLines)
		}
		if rt.logRings.Embed != nil {
			rt.logRings.Embed.Resize(loaded.Log.ProcMaxLines)
		}
		result.LiveApplied = true
	}

	return result
}
