package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/ui"
)

// handleProjectSwitch optionally reloads llama-server when the active project
// changes. The caller must hold rt.mu on entry and quiesce live memory/API work
// before calling so sessions are committed under the previous project manager.
//
// oldModel and newModel are the effective models the caller already computed
// for the pre-apply and to-be-applied configs (rt.effectiveModelFor(&old) and
// rt.effectiveModelFor(loaded) in config.go) — passed in rather than
// recomputed here for two reasons. First, so this can tell whether the
// *global* Model.Port (and Verbose) fields themselves changed in the same
// apply as the switch: neither is ever project-specific (see
// config.ModelConfigEqual's own comment), so that comparison does not depend
// on which project ends up active. Second, and just as important: oldModel
// is exactly what rt.effectiveModelFor already falls back to (the global
// Model config, unmodified) when the project store cannot resolve the
// source project — a re-resolution done locally in this function instead
// used to bail out of the port/verbose sync entirely on that same failure,
// even though the inference client had already moved to newModel's port
// unconditionally (config.go's reconfigureProcesses). Using the caller's
// already-computed, already-fallback-safe oldModel here closes that gap:
// nothing this function does can silently skip syncing llama-server just
// because the project store had a bad moment.
//
// Returns true when it actually reconfigured llama-server, so a caller that
// goes on to attempt a memory/API rebuild and finds it fails knows whether
// there is a process move to undo: reverting only the generic
// reconfigureProcesses call and not this one would leave llama-server on the
// destination project's model while the restored (source project's) memory
// graph expects the source's.
func (rt *Runtime) handleProjectSwitch(ctx context.Context, uiServer *ui.Server, oldConfig, newConfig *config.Config, oldModel, newModel config.ModelConfig) bool {
	llamaPolicy := rt.cfg.Project.LlamaOnSwitch

	if llamaPolicy != "reload" {
		slog.Info("project switch: keeping current llama-server (llama_on_switch=keep)")
		// Compute whether there is a model mismatch to surface in the UI.
		// This is a display-only comparison, so it deliberately still uses
		// ModelConfigEqual's Port/Verbose exclusion: a pure port or verbose
		// difference is not a "different model" from the user's point of
		// view and should not raise the mismatch banner.
		dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
		if err == nil {
			dstModel := config.EffectiveModel(newConfig, dstProj)
			if !config.ModelConfigEqual(oldModel, dstModel) {
				uiServer.SetModelMismatch(true, oldModel.ModelPath, dstModel.ModelPath)
			} else {
				uiServer.SetModelMismatch(false, "", "")
			}
		} else {
			uiServer.SetModelMismatch(false, "", "")
		}

		// "keep" governs the *model* llama-server serves across this switch,
		// not whether it listens on the port (or runs with the verbose flag)
		// the rest of this apply says it should. A direct edit to either
		// global field, saved in the same apply as a switch, must still
		// take effect regardless of policy — reconfigureProcesses's own
		// port-driven inference-client swap (config.go) is unconditional on
		// projectSwitching for exactly this reason, since the port is
		// global. Without this, the client would move to newModel.Port
		// while llama-server stayed on oldModel's (old) port, pointing
		// every request at nothing. keepModel starts from oldModel — the
		// model actually running — and adopts only the fields that are
		// global, never project-specific, from newModel; comparing the
		// result to oldModel with a plain == (ModelConfig has no
		// incomparable fields) is exactly "did any global field actually
		// change," and covers Port and Verbose alike rather than
		// special-casing Port only.
		keepModel := oldModel
		keepModel.Port = newModel.Port
		keepModel.Verbose = newModel.Verbose
		if rt.llamaMgr != nil && keepModel != oldModel {
			slog.Info("project switch: keeping model, syncing changed global fields",
				"model_path", keepModel.ModelPath, "port", keepModel.Port, "verbose", keepModel.Verbose)
			rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(keepModel) }, llamaHealthURL(keepModel))
			return true
		}
		return false
	}

	dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
	if err != nil {
		slog.Warn("project switch: cannot resolve destination project, skipping reload", "err", err)
		return false
	}
	dstModel := config.EffectiveModel(newConfig, dstProj)

	// dstModel already carries newConfig's global Port and Verbose (neither
	// is ever a project override, and EffectiveModel starts from cfg.Model),
	// so a plain == against oldModel — which likewise already reflects
	// everything currently running, model identity and global fields alike
	// — is exactly "would anything about the running process's args differ
	// after this reload." ModelConfigEqual is the wrong tool here: it
	// exists specifically to ignore Port and Verbose for a different
	// caller's purpose, so using it (even with a bolted-on Port check) left
	// a same-model, Verbose-only divergence unnoticed, and a project the
	// store could not resolve used to force a reload defensively instead of
	// this comparison simply working — oldModel cannot fail to resolve.
	if oldModel == dstModel {
		slog.Info("project switch: effective model unchanged, skipping reload")
		return false
	}

	slog.Info("project switch: reloading llama-server",
		"from_slug", oldConfig.Project.ActiveProjectSlug,
		"to_slug", newConfig.Project.ActiveProjectSlug,
		"new_model_path", dstModel.ModelPath,
	)

	if rt.reqQueue != nil {
		if err := rt.reqQueue.Restart(ctx); err != nil {
			slog.Error("project switch: queue restart failed", "err", err)
		}
	}
	reconfigured := false
	if rt.llamaMgr != nil {
		rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(dstModel) }, llamaHealthURL(dstModel))
		reconfigured = true
	}

	uiServer.SetModelMismatch(false, "", "")
	return reconfigured
}

// resolveProject looks up a project by slug from the project store.
// Returns nil, error when the store is unavailable or the project is
// not found.
func (rt *Runtime) resolveProject(slug string) (*project.Project, error) {
	if slug == "" {
		slug = project.GlobalSlug
	}
	if rt.projectStore == nil {
		return nil, fmt.Errorf("project store not available")
	}
	proj, err := rt.projectStore.Get(slug)
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", slug, err)
	}
	return &proj, nil
}
