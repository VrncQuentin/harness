package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/ui"
)

// handleProjectSwitch flushes the active session (so it is committed under
// the old project) and optionally reloads llama-server when the active
// project changes. The caller must hold rt.mu on entry; the method
// temporarily releases it during session flush to avoid deadlock with the
// summarizer's config accessor (summarizerPromptFn acquires rt.mu).
func (rt *Runtime) handleProjectSwitch(ctx context.Context, uiServer *ui.Server, oldConfig, newConfig *config.Config) {
	llamaPolicy := rt.cfg.Project.LlamaOnSwitch

	if mgr := rt.SessionManager(); mgr != nil {
		rt.mu.Unlock()
		flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := mgr.FlushAll(flushCtx)
		cancel()
		rt.mu.Lock()
		if err != nil {
			slog.Warn("project switch: session flush", "err", err)
		}
	}

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
				return
			}
		}
		uiServer.SetModelMismatch(false, "", "")
		return
	}

	dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
	if err != nil {
		slog.Warn("project switch: cannot resolve destination project, skipping reload", "err", err)
		return
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
			return
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
	if rt.llamaMgr != nil {
		rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(dstModel) }, llamaHealthURL(dstModel))
	}

	uiServer.SetModelMismatch(false, "", "")
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
