package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/ui"
)

// ErrActiveProjectRepoMove is returned when an edit would move the active
// project's memory-repository boundary while the installed generation still
// targets it. The runtime refuses before any metadata or filesystem mutation:
// the session manager, episode index, and generation readers are bound to the
// current physical repository, and an alias spelling must not manufacture a
// move where physical identity says the paths are the same repository.
var ErrActiveProjectRepoMove = errors.New("active project memory repo cannot be moved while it is in use")

// EditProject is the Runtime-owned project-update surface used by the UI's
// /projects/edit handler. It replaces the handler's direct construction and
// execution of project.Workflow with one transaction serialized end-to-end by
// applyMu, so an edit cannot interleave with a config apply or a shutdown:
//
//   - the active project's memory-repository boundary cannot be moved while
//     the installed generation still targets it (refused before any metadata
//     or filesystem mutation, using physical identity so aliases cannot
//     manufacture a move);
//   - the old/live state is read exclusively from the recorded applied state,
//     never reconstructed from the store this edit is about to mutate;
//   - an active-project display/model-override edit re-applies the live
//     system through the same transaction boundary, so the recorded applied
//     state is compared against the freshly-mutated store;
//   - an inactive-project repository move continues through the rooted
//     MoveProjectRepo workflow, preserving its rollback behavior on
//     initialization or move failure.
func (rt *Runtime) EditProject(
	ctx context.Context,
	uiServer *ui.Server,
	events chan proc.Event,
	metricsStore metrics.Store,
	input project.UpdateInput,
	memoryRepoMode string,
) (project.Project, error) {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

	if rt.projectStore == nil {
		return project.Project{}, errors.New("project store not available")
	}

	rt.mu.Lock()
	applied := rt.applied
	rt.mu.Unlock()
	if applied == nil {
		return project.Project{}, errors.New("runtime is not started; cannot edit projects")
	}

	current, err := rt.projectStore.Get(input.Slug)
	if err != nil {
		return project.Project{}, fmt.Errorf("edit project %q: %w", input.Slug, err)
	}

	// Settle the repository identity once, before any mutation, backed by a
	// handle-bound proof of the destination. A failure to answer "is this the
	// same repository" aborts the edit rather than folding into "different"
	// (which would run a move against a repository that might be the
	// destination itself) or "same" (which would silently drop a repointing
	// the user asked for). The proof is re-verified at mutation time, so an
	// alias repointed after the decision fails closed instead of authorizing a
	// mutation of a changed repository.
	workflow := project.NewWorkflow(rt.projectStore, memory.ProjectRepoManager{})
	settled, err := workflow.SettleUpdate(input)
	if err != nil {
		return project.Project{}, fmt.Errorf("identify memory repo path: %w", err)
	}
	memoryRepoChanged := !settled.SameRepo
	if memoryRepoChanged {
		switch memoryRepoMode {
		case project.MemoryRepoModeMove, project.MemoryRepoModeFresh:
		default:
			return project.Project{}, errors.New("choose whether to move existing memory data or start fresh")
		}
	}
	if rt.afterProjectIdentity != nil {
		rt.afterProjectIdentity()
	}

	// Refuse to move the active project's memory repository before any
	// metadata or filesystem mutation: the installed generation, session
	// manager, and episode index still target the current boundary.
	if input.Slug == applied.activeSlug && memoryRepoChanged {
		return project.Project{}, ErrActiveProjectRepoMove
	}

	updated, err := workflow.ApplyUpdate(input, memoryRepoMode, settled)
	if err != nil {
		return project.Project{}, err
	}

	// An active-project edit (display name or model override) must take live
	// effect through the same transaction boundary: re-apply the config so the
	// reload decision compares the freshly-mutated store contents with the
	// recorded applied state (never with a store-derived reconstruction of the
	// old values) and the live processes follow the new effective model.
	if input.Slug == applied.activeSlug {
		result := rt.applyConfigLocked(ctx, uiServer, events, metricsStore)
		if result.Err != nil {
			// The re-apply failed: the installed generation and recorded
			// applied state are untouched, so the persisted project row must
			// be restored to match. The failure is reported to the handler.
			rollback := project.UpdateInput{
				Slug:           current.Slug,
				DisplayName:    current.DisplayName,
				MemoryRepoPath: current.MemoryRepoPath,
				ModelBinary:    current.ModelBinary,
				ModelPath:      current.ModelPath,
				ModelCtxSize:   current.ModelCtxSize,
				ModelGPULayers: current.ModelGPULayers,
				ModelNParallel: current.ModelNParallel,
			}
			if _, rollbackErr := rt.projectStore.Update(rollback); rollbackErr != nil {
				return project.Project{}, fmt.Errorf("apply project edit: %v; rollback project metadata: %v", result.Err, rollbackErr)
			}
			return project.Project{}, fmt.Errorf("apply project edit: %w", result.Err)
		}
	}

	return updated, nil
}
