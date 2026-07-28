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
// Returns true when it actually reconfigured llama-server, so a caller that
// goes on to attempt a memory/API rebuild and finds it fails knows whether
// there is a process move to undo: reverting only the generic
// reconfigureProcesses call and not this one would leave llama-server on the
// destination project's model while the restored (source project's) memory
// graph expects the source's.
func (rt *Runtime) handleProjectSwitch(ctx context.Context, uiServer *ui.Server, oldConfig, newConfig *config.Config) bool {
	llamaPolicy := rt.cfg.Project.LlamaOnSwitch

	if llamaPolicy != "reload" {
		slog.Info("project switch: keeping current llama-server (llama_on_switch=keep)")
		// Compute whether there is a model mismatch to surface in the UI.
		srcProj, _ := rt.resolveProject(oldConfig.Project.ActiveProjectSlug)
		srcModel := config.EffectiveModel(oldConfig, srcProj)
		dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
		if err == nil {
			dstModel := config.EffectiveModel(newConfig, dstProj)
			if !config.ModelConfigEqual(srcModel, dstModel) {
				uiServer.SetModelMismatch(true, srcModel.ModelPath, dstModel.ModelPath)
				return false
			}
		}
		uiServer.SetModelMismatch(false, "", "")
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
		if config.ModelConfigEqual(srcModel, dstModel) {
			slog.Info("project switch: effective model unchanged, skipping reload")
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
