package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/ui"
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
	rt.cfg = *loaded
	rt.refreshProjectDirectoryWarnings(uiServer)

	var result ui.ApplyResult

	if !rt.started {
		slog.Info("starting services", "model_port", loaded.Model.Port, "embed_port", loaded.Embedder.Port)
		rt.startServices(ctx, uiServer, events, metricsStore)
		rt.startMemoryAndAPI(ctx, uiServer, metricsStore)
		result.LiveApplied = true
	} else {
		if old.Model != loaded.Model {
			slog.Info("reconfiguring llama-server", "old_port", old.Model.Port, "new_port", loaded.Model.Port)
			rt.llamaMgr.Reconfigure(func() (string, []string) {
				return proc.LlamaArgs(
					loaded.Model.Binary,
					loaded.Model.ModelPath,
					loaded.Model.CtxSize,
					loaded.Model.GPULayers,
					loaded.Model.NParallel,
					loaded.Model.Port,
					loaded.Model.Verbose,
					loaded.Model.CacheTypeK,
					loaded.Model.CacheTypeV,
				)
			}, fmt.Sprintf("http://127.0.0.1:%d/health", loaded.Model.Port))
			result.LiveApplied = true
		}
		if old.Embedder != loaded.Embedder {
			slog.Info("reconfiguring embedder", "old_port", old.Embedder.Port, "new_port", loaded.Embedder.Port)
			rt.embedMgr.Reconfigure(func() (string, []string) {
				return proc.EmbedderArgs(
					loaded.Embedder.Binary,
					loaded.Embedder.ModelPath,
					loaded.Embedder.Port,
					loaded.Embedder.Verbose,
				)
			}, fmt.Sprintf("http://127.0.0.1:%d/health", loaded.Embedder.Port))
			result.LiveApplied = true
		}
		if old.Model.Port != loaded.Model.Port && rt.reqQueue != nil {
			client := rt.newInferenceClient()
			rt.inferClient = client
			rt.reqQueue.SetClient(client)
		}

		if old.Prompt != loaded.Prompt ||
			old.API != loaded.API ||
			old.Loop != loaded.Loop ||
			old.Agent.Active != loaded.Agent.Active ||
			old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug {
			// Project switch: flush current session and optionally
			// reload llama-server before rebuilding memory services.
			if old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug {
				rt.handleProjectSwitch(ctx, uiServer, &old, loaded)
			}
			slog.Info("rebuilding memory and api services")
			rt.stopMemoryAndAPI(uiServer)
			rt.startMemoryAndAPI(ctx, uiServer, metricsStore)
			result.LiveApplied = true
		}
	}

	if old.UI.Port != loaded.UI.Port {
		result.RestartNeeded = append(result.RestartNeeded, "UI port")
	}
	if old.Queue.MaxDepth != loaded.Queue.MaxDepth {
		result.RestartNeeded = append(result.RestartNeeded, "queue max depth")
	}
	if old.Queue.WALPath != loaded.Queue.WALPath {
		result.RestartNeeded = append(result.RestartNeeded, "queue WAL path")
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
