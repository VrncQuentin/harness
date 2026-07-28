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
	// Held for this whole call, well past the windows where rt.mu itself is
	// released (draining, API shutdown) — see Runtime.applyMu's own doc
	// comment for why mu alone cannot serialize two concurrent callers here.
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

	// Stop takes applyMu for its entire call and sets stopping before doing
	// anything else (see Stop's doc comment in lifecycle.go), so observing it
	// here means Stop is already underway — or already finished — and it
	// would be unsafe to quiesce, reconfigure, or reopen admission against
	// services Stop is tearing down (or has already torn down).
	if rt.stopping {
		return ui.ApplyResult{}
	}

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
	// Port is a process-level flag, never part of a project's model
	// override (config.ModelConfigEqual's own comment: the harness runs
	// exactly one llama-server, whose port comes from the global config) —
	// so this is only ever true because of a direct edit to Model.Port
	// itself, never as a side effect of switching projects.
	modelEndpointChanged := oldModel.Port != newModel.Port

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
		// oldModel != newModel and old.Embedder != loaded.Embedder are used
		// directly here, not just modelEndpointChanged/embedderEndpointChanged
		// (port-only): reconfigureProcesses below reacts to any field in
		// either changing, not just the port, so needsRebuild must trigger
		// quiescing for the same set of changes it reacts to. Gating this on
		// port alone let a same-port model-path, context-size, or embedder
		// change reach the !needsRebuild branch, where reconfigureProcesses
		// runs immediately with nothing drained first -- reconfiguring (and
		// for llama-server, restarting) a process still serving active
		// requests.
		projectSwitching := old.Project.ActiveProjectSlug != loaded.Project.ActiveProjectSlug
		needsRebuild := old.Prompt != loaded.Prompt ||
			old.API != loaded.API ||
			old.Loop != loaded.Loop ||
			old.Agent.Active != loaded.Agent.Active ||
			projectSwitching ||
			oldModel != newModel ||
			old.Embedder != loaded.Embedder ||
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
		// llamaMoved/embedderMoved/clientSwapped track which of this apply's
		// forward reconfigures actually happened, so a rebuild that gets that
		// far and then fails can undo exactly those and no others: calling
		// proc.Manager.Reconfigure unconditionally restarts the process even
		// when the args have not changed (Reconfigure -> Restart, always), so
		// undoing a component that never moved would cause a needless,
		// disruptive restart of an already-correct process.
		llamaMoved := false
		embedderMoved := false
		clientSwapped := false

		reconfigureProcesses := func() {
			if oldModel != newModel && !projectSwitching {
				// A project switch's own llama decision -- including
				// honoring llama_on_switch=keep -- belongs entirely to
				// handleProjectSwitch, called separately below with its own,
				// more specific comparison of effective models. Model.Port is
				// never project-specific (see config.ModelConfigEqual's own
				// comment: the harness runs exactly one llama-server, whose
				// port comes from the global config), so oldModel != newModel
				// across a switch almost always reflects a *different*
				// per-project model override rather than a raw config edit —
				// reconfiguring here first would move the process before
				// handleProjectSwitch's decision runs, defeating "keep"
				// in exactly that common case.
				slog.Info("reconfiguring llama-server", "old_port", oldModel.Port, "new_port", newModel.Port)
				rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(newModel) }, llamaHealthURL(newModel))
				llamaMoved = true
				result.LiveApplied = true
			}
			if old.Embedder != loaded.Embedder {
				slog.Info("reconfiguring embedder", "old_port", old.Embedder.Port, "new_port", loaded.Embedder.Port)
				rt.embedMgr.Reconfigure(func() (string, []string) {
					return embedderArgsForConfig(loaded.Embedder)
				}, embedderHealthURL(loaded.Embedder))
				embedderMoved = true
				result.LiveApplied = true
			}
			if modelEndpointChanged && rt.reqQueue != nil {
				// Port is global, never project-specific, so this is
				// unaffected by projectSwitching: it only fires when the raw
				// Model.Port field itself changed. newModel, not rt.cfg, is
				// the target: rt.cfg is not committed to loaded until the
				// caller decides this reconfigure is actually going to stick
				// (see below), so a client built by reading rt.cfg here would
				// still point at the old port.
				client := rt.newInferenceClientForModel(newModel)
				rt.inferClient = client
				rt.reqQueue.SetClient(client)
				clientSwapped = true
			}
		}

		// undoProcessReconfigure reverses exactly the components this apply
		// actually moved forward, back to old/oldModel. Called only when a
		// rebuild that reconfigureProcesses (and possibly handleProjectSwitch)
		// already ran ahead of turns out to fail: the memory/git generation
		// is being restored to `old` in that case, and pairing it with
		// processes still on the new config would be coherent for neither
		// generation -- the same reasoning reconfigureProcesses documents for
		// running only after a successful quiesce applies just as much to
		// running this in reverse after a failed rebuild.
		undoProcessReconfigure := func() {
			if llamaMoved {
				slog.Info("restoring llama-server after failed rebuild", "port", oldModel.Port)
				rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(oldModel) }, llamaHealthURL(oldModel))
			}
			if embedderMoved {
				slog.Info("restoring embedder after failed rebuild", "port", old.Embedder.Port)
				rt.embedMgr.Reconfigure(func() (string, []string) {
					return embedderArgsForConfig(old.Embedder)
				}, embedderHealthURL(old.Embedder))
			}
			if clientSwapped && rt.reqQueue != nil {
				client := rt.newInferenceClientForModel(oldModel)
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
			// reconfigure to disrupt. Sessions are flushed here, before the
			// process reconfigure that follows, so any last-minute
			// summarization still runs against the model the conversation
			// was actually held with, not the one this reload is about to
			// switch to.
			rt.flushSessionsForReload(ctx)
			reconfigureProcesses()
			rt.cfg = *loaded
			rt.refreshProjectDirectoryWarnings(uiServer)
			// Project switch optionally reloads llama-server before rebuilding
			// memory services. Live work has already been quiesced above so it
			// is committed under the previous project manager.
			if projectSwitching {
				if rt.handleProjectSwitch(ctx, uiServer, &old, loaded, newModel) {
					// handleProjectSwitch's own reload (llama_on_switch=reload,
					// or a llama_on_switch=keep move to a changed global port)
					// also moved llama-server forward; undoProcessReconfigure
					// must know to revert it too if the rebuild below fails,
					// alongside anything reconfigureProcesses itself moved.
					llamaMoved = true
				}
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
				// must describe one generation, not two. The process
				// managers and inference client must follow the same way:
				// reconfigureProcesses (and possibly handleProjectSwitch)
				// already moved them to the new config before this rebuild
				// was attempted, and the restored generation was built for
				// the old one.
				rt.cfg = old
				rt.refreshProjectDirectoryWarnings(uiServer)
				undoProcessReconfigure()
				// reconfigureProcesses may have already set this true before
				// the rebuild was attempted; since everything it did has now
				// been undone above alongside the rest of this generation,
				// the apply as a whole did not stick, and the result must
				// say so — not report success for a configuration that was
				// just rolled back. Anything unrelated to this branch (the
				// log-ring resize below, say) still sets it independently.
				result.LiveApplied = false
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
