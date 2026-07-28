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
// newModel is the just-computed effective model for the config being applied
// (rt.effectiveModelFor(loaded) in config.go) — passed in rather than
// recomputed so this can tell whether the *global* Model.Port field itself
// changed in the same apply as the switch. Port is never project-specific
// (see config.ModelConfigEqual's own comment: the harness runs exactly one
// llama-server, whose port comes from the global config), so that comparison
// does not depend on which project ends up active.
//
// Returns true when it actually reconfigured llama-server, so a caller that
// goes on to attempt a memory/API rebuild and finds it fails knows whether
// there is a process move to undo: reverting only the generic
// reconfigureProcesses call and not this one would leave llama-server on the
// destination project's model while the restored (source project's) memory
// graph expects the source's.
func (rt *Runtime) handleProjectSwitch(ctx context.Context, uiServer *ui.Server, oldConfig, newConfig *config.Config, newModel config.ModelConfig) bool {
	llamaPolicy := rt.cfg.Project.LlamaOnSwitch

	if llamaPolicy != "reload" {
		slog.Info("project switch: keeping current llama-server (llama_on_switch=keep)")
		// Compute whether there is a model mismatch to surface in the UI.
		srcProj, srcErr := rt.resolveProject(oldConfig.Project.ActiveProjectSlug)
		var srcModel config.ModelConfig
		if srcErr == nil {
			srcModel = config.EffectiveModel(oldConfig, srcProj)
		}
		dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
		if err == nil && srcErr == nil {
			dstModel := config.EffectiveModel(newConfig, dstProj)
			if !config.ModelConfigEqual(srcModel, dstModel) {
				uiServer.SetModelMismatch(true, srcModel.ModelPath, dstModel.ModelPath)
			} else {
				uiServer.SetModelMismatch(false, "", "")
			}
		} else {
			uiServer.SetModelMismatch(false, "", "")
		}

		// "keep" governs the *model* llama-server serves across this switch,
		// not whether it listens on the port the rest of this apply says it
		// should. A direct edit to the global Model.Port field, saved in the
		// same apply as a switch, must still take effect regardless of
		// policy — reconfigureProcesses's own port-driven inference-client
		// swap (config.go) is unconditional on projectSwitching for exactly
		// this reason, since the port is global. Without this, the client
		// would move to newModel.Port while llama-server stayed on
		// srcModel's (old) port, pointing every request at nothing.
		// Reconfiguring with srcModel — the model actually being served —
		// with only the port swapped keeps "keep" honest: the destination
		// project's model is never loaded here.
		if srcErr == nil && rt.llamaMgr != nil && srcModel.Port != newModel.Port {
			keepModel := srcModel
			keepModel.Port = newModel.Port
			slog.Info("project switch: keeping model, moving to the new port",
				"model_path", keepModel.ModelPath, "port", keepModel.Port)
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
	srcProj, srcErr := rt.resolveProject(oldConfig.Project.ActiveProjectSlug)
	if srcErr != nil {
		slog.Warn("project switch: cannot resolve source project, forcing reload",
			"slug", oldConfig.Project.ActiveProjectSlug, "err", srcErr)
	} else {
		srcModel := config.EffectiveModel(oldConfig, srcProj)
		// ModelConfigEqual deliberately ignores Port (see its own doc
		// comment), so it alone cannot tell "same model" from "same model,
		// different port" apart. Under this policy the destination project's
		// model is always the one that ends up loaded, so a port-only
		// divergence still requires a real Reconfigure — skipping it here
		// would leave llama-server listening on the old port while
		// reconfigureProcesses's inference-client swap (config.go) has
		// already moved every request to dstModel.Port.
		if config.ModelConfigEqual(srcModel, dstModel) && srcModel.Port == dstModel.Port {
			slog.Info("project switch: effective model and port unchanged, skipping reload")
			return false
		}
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
