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

	var result ui.ApplyResult

	if !rt.started {
		// Nothing to protect yet -- there is no previous generation a wrong
		// commit could leave stranded -- so rt.cfg can be set immediately;
		// startServices/startMemoryAndAPI read it live.
		rt.cfg = *loaded
		rt.refreshProjectDirectoryWarnings(uiServer)
		slog.Info("starting services", "model_port", newModel.Port, "embed_port", loaded.Embedder.Port)
		rt.startServices(ctx, uiServer, events, metricsStore)
		rt.startMemoryAndAPI(ctx, uiServer, metricsStore)
		result.LiveApplied = true
	} else {
		needsMemoryAPIRetry := rt.memoryAPIUnavailable()
		needsRebuild := old.Prompt != loaded.Prompt ||
			old.API != loaded.API ||
			old.Loop != loaded.Loop ||
			old.Agent.Active != loaded.Agent.Active ||
			old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug ||
			modelEndpointChanged ||
			embedderEndpointChanged ||
			needsMemoryAPIRetry

		// reconfigureProcesses restarts llama-server/the embedder and swaps
		// the inference client's target port, all independent of whether the
		// memory/API service graph also needs rebuilding. When it does, this
		// must run only after quiescing has actually succeeded, not before
		// it: an in-flight task or session-flush summarization still using
		// the old model would otherwise have its target change out from under
		// it mid-request, and an aborted rebuild (a failed drain or API
		// shutdown) would otherwise leave the old memory/API generation
		// paired with processes that have already moved to the new config —
		// coherent for neither the old generation nor the new one.
		reconfigureProcesses := func() {
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
				// newModel, not rt.cfg, is the target: rt.cfg is not committed
				// to loaded until the caller decides this reconfigure is
				// actually going to stick (see below), so a client built by
				// reading rt.cfg here would still point at the old port.
				client := rt.newInferenceClientForModel(newModel)
				rt.inferClient = client
				rt.reqQueue.SetClient(client)
			}
		}

		if !needsRebuild {
			reconfigureProcesses()
			rt.cfg = *loaded
			rt.refreshProjectDirectoryWarnings(uiServer)
		} else if err := rt.quiesceMemoryAndAPI(ctx, uiServer); err != nil {
			// The UI generation gate has already reopened itself; nothing
			// has been quiesced or torn down, so the safe choice is to
			// leave the current generation running and let a later
			// retry (manual or the next config save) try again, rather
			// than rebuild out from under a request the drain could not
			// wait for. rt.cfg is deliberately left as `old` here, not
			// `loaded`: committing the new config while the service
			// graph backing it was never rebuilt would let every
			// component that reads rt.cfg live -- the task runner's
			// sandbox-root resolution, notably -- observe a project or
			// prompt config the still-running old generation was never
			// built for, e.g. resolving sandbox roots for a new project
			// slug while memory operations still run against the old
			// project's repo. The processes are not reconfigured either,
			// for the same reason: nothing has been quiesced yet.
			slog.Warn("runtime reload: aborting memory/API rebuild, UI request drain failed", "err", err)
		} else if err := rt.shutdownAPIServerForReload(ctx); err != nil {
			// shutdownAPIServerForReload has already cleared rt.apiServer
			// and closed its listener regardless of this error -- that part
			// cannot be undone -- but the memory/git generation is still
			// untouched, so the same "leave the current generation running"
			// choice applies to rt.cfg, the process managers, and the UI gate
			// as it does above.
			slog.Warn("runtime reload: aborting memory/API rebuild, API server shutdown failed", "err", err)
			uiServer.ResumeGenerationAdmission()
		} else {
			// Quiescing succeeded, so this reload is committed: the old
			// generation's in-flight work is drained, and there is nothing
			// left running against the old model/embedder for a process
			// reconfigure to disrupt.
			reconfigureProcesses()
			rt.cfg = *loaded
			rt.refreshProjectDirectoryWarnings(uiServer)
			// Project switch optionally reloads llama-server before rebuilding
			// memory services. Live work has already been quiesced above so it
			// is committed under the previous project manager.
			if old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug {
				rt.handleProjectSwitch(ctx, uiServer, &old, loaded)
			}
			slog.Info("rebuilding memory and api services")
			snapshot := rt.snapshotMemoryAndAPI(uiServer)
			rt.stopMemoryAndAPI(uiServer)
			if rt.startMemoryAndAPI(ctx, uiServer, metricsStore) {
				result.LiveApplied = true
				// The replacement is live, so the generation it replaced is
				// now unreachable and its pinned handles can go.
				snapshot.closeReplaced()
			} else {
				rt.restoreMemoryAndAPI(uiServer, snapshot)
				// The rebuild attempt itself failed and the previous
				// generation's service graph was restored -- rt.cfg
				// follows it back to `old` for the same reason it was
				// never advanced past `old` on the drain-timeout path
				// above: the live services and the config a caller reads
				// must describe one generation, not two.
				rt.cfg = old
				rt.refreshProjectDirectoryWarnings(uiServer)
			}
			// The UI generation gate was left closed by a successful
			// drain above; reopen it now that deps.SetServiceDeps points
			// at either the new generation or the restored old one.
			uiServer.ResumeGenerationAdmission()
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
