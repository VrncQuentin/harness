package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/ui"
)

// handleProjectSwitch flushes the active session (so it is committed under
// the old project) and optionally reloads llama-server when the active
// project changes. Caller must hold rt.mu.
func (rt *Runtime) handleProjectSwitch(ctx context.Context, uiServer *ui.Server, oldConfig, newConfig *config.Config) {
	// Flush the live session under the old project so its episode lands
	// in the old project's directory. The manager is replaced by the
	// subsequent startMemoryAndAPI call.
	if mgr := rt.SessionManager(); mgr != nil {
		flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := mgr.FlushAll(flushCtx); err != nil {
			slog.Warn("project switch: session flush", "err", err)
		}
		cancel()
	}
	if rt.cfg.Project.LlamaOnSwitch != "reload" {
		slog.Info("project switch: keeping current llama-server (llama_on_switch=keep)")
		return
	}

	dstProj, err := rt.resolveProject(newConfig.Project.ActiveProjectSlug)
	if err != nil {
		slog.Warn("project switch: cannot resolve destination project, skipping reload", "err", err)
		return
	}

	dstModel := config.EffectiveModel(newConfig, dstProj)
	srcModel := config.EffectiveModel(oldConfig, rt.lookupProject(oldConfig.Project.ActiveProjectSlug))

	if config.ModelConfigEqual(srcModel, dstModel) {
		slog.Info("project switch: effective model unchanged, skipping reload")
		return
	}

	slog.Info("project switch: reloading llama-server",
		"from_slug", oldConfig.Project.ActiveProjectSlug,
		"to_slug", newConfig.Project.ActiveProjectSlug,
		"new_model_path", dstModel.ModelPath,
	)

	if rt.reqQueue != nil {
		rt.reqQueue.Stop()
	}
	if rt.llamaMgr != nil {
		rt.llamaMgr.Reconfigure(func() (string, []string) {
			return proc.LlamaArgs(
				dstModel.Binary,
				dstModel.ModelPath,
				dstModel.CtxSize,
				dstModel.GPULayers,
				dstModel.NParallel,
				dstModel.Port,
				dstModel.Verbose,
				dstModel.CacheTypeK,
				dstModel.CacheTypeV,
			)
		}, fmt.Sprintf("http://127.0.0.1:%d/health", dstModel.Port))
	}
	if rt.reqQueue != nil {
		if err := rt.reqQueue.Start(ctx); err != nil {
			slog.Error("project switch: queue restart failed", "err", err)
		}
	}
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
	ps, ok := rt.projectStore.(project.Store)
	if !ok {
		return nil, fmt.Errorf("project store does not implement Get")
	}
	proj, err := ps.Get(slug)
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", slug, err)
	}
	return &proj, nil
}

// lookupProject is like resolveProject but returns nil on any error;
// the caller gets a nil project which treats all overrides as absent.
func (rt *Runtime) lookupProject(slug string) *project.Project {
	proj, err := rt.resolveProject(slug)
	if err != nil {
		slog.Warn("project switch: cannot resolve source project", "slug", slug, "err", err)
		return nil
	}
	return proj
}
