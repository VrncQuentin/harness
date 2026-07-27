package project

import (
	"errors"
	"fmt"
)

const (
	MemoryRepoModeFresh = "fresh"
	MemoryRepoModeMove  = "move"
)

// MemoryRepoManager owns filesystem workflows for project memory repos. It is
// intentionally small so project workflows can coordinate metadata and repo
// state without importing internal/memory and creating a package cycle.
type MemoryRepoManager interface {
	EnsureProjectRepo(root string, global bool) error
	MoveProjectRepo(src, dst string, global bool) error
	// SameProjectRepoPath answers whether two paths name one repository by
	// physical identity. It returns an error when identity cannot be
	// established, which Workflow must not fold into "different": the two
	// branches that follow are "leave the repo alone" and "copy over the
	// destination", and guessing between them on an unresolved path is how a
	// repository gets copied onto itself.
	SameProjectRepoPath(a, b string) (bool, error)
}

// WorkflowStore is the project metadata surface required by Workflow.
type WorkflowStore interface {
	Get(slug string) (Project, error)
	Create(input CreateInput) (Project, error)
	Update(input UpdateInput) (Project, error)
}

type deleteStore interface {
	Delete(slug string) error
}

// Workflow coordinates project metadata changes with memory repository setup.
// HTTP handlers should parse requests and call this service instead of owning
// cross-package sequencing and compensation logic themselves.
type Workflow struct {
	Store WorkflowStore
	Repos MemoryRepoManager
}

// NewWorkflow builds a project workflow coordinator over metadata and repo services.
func NewWorkflow(store WorkflowStore, repos MemoryRepoManager) *Workflow {
	return &Workflow{Store: store, Repos: repos}
}

// Create persists project metadata and initializes its memory repo, rolling back
// metadata when repo setup fails.
func (w *Workflow) Create(input CreateInput) (Project, error) {
	if w.Store == nil {
		return Project{}, errors.New("project: store not configured")
	}
	if w.Repos == nil {
		return Project{}, errors.New("project: memory repo manager not configured")
	}
	created, err := w.Store.Create(input)
	if err != nil {
		return Project{}, err
	}
	if err := w.Repos.EnsureProjectRepo(created.MemoryRepoPath, created.Slug == GlobalSlug); err != nil {
		if ds, ok := w.Store.(deleteStore); ok {
			if rollbackErr := ds.Delete(created.Slug); rollbackErr != nil {
				return Project{}, fmt.Errorf("initialize memory repo: %w; rollback project metadata: %v", err, rollbackErr)
			}
		}
		return Project{}, fmt.Errorf("initialize memory repo: %w", err)
	}
	return created, nil
}

// Update persists project metadata and reconciles memory repo path changes with
// rollback when repo initialization or migration fails.
func (w *Workflow) Update(input UpdateInput, memoryRepoMode string) (Project, error) {
	if w.Store == nil {
		return Project{}, errors.New("project: store not configured")
	}
	if w.Repos == nil {
		return Project{}, errors.New("project: memory repo manager not configured")
	}
	current, err := w.Store.Get(input.Slug)
	if err != nil {
		return Project{}, err
	}

	// Identity is settled before the metadata update, not after it. Resolving
	// later would leave a failure to answer "is this the same repository"
	// stranded between a committed metadata change and an untouched repo.
	memoryRepoChanged := false
	if input.MemoryRepoPath != "" && current.MemoryRepoPath != "" {
		same, err := w.Repos.SameProjectRepoPath(input.MemoryRepoPath, current.MemoryRepoPath)
		if err != nil {
			return Project{}, fmt.Errorf("identify memory repo path: %w", err)
		}
		memoryRepoChanged = !same
	}
	if memoryRepoChanged {
		switch memoryRepoMode {
		case MemoryRepoModeMove, MemoryRepoModeFresh:
		default:
			return Project{}, errors.New("choose whether to move existing memory data or start fresh")
		}
	}

	updated, err := w.Store.Update(input)
	if err != nil {
		return Project{}, err
	}
	if !memoryRepoChanged {
		return updated, nil
	}

	var repoErr error
	switch memoryRepoMode {
	case MemoryRepoModeMove:
		repoErr = w.Repos.MoveProjectRepo(current.MemoryRepoPath, updated.MemoryRepoPath, updated.Slug == GlobalSlug)
	case MemoryRepoModeFresh:
		repoErr = w.Repos.EnsureProjectRepo(updated.MemoryRepoPath, updated.Slug == GlobalSlug)
	}
	if repoErr == nil {
		return updated, nil
	}

	rollback := UpdateInput{
		Slug:           current.Slug,
		DisplayName:    current.DisplayName,
		MemoryRepoPath: current.MemoryRepoPath,
		ModelBinary:    current.ModelBinary,
		ModelPath:      current.ModelPath,
		ModelCtxSize:   current.ModelCtxSize,
		ModelGPULayers: current.ModelGPULayers,
		ModelNParallel: current.ModelNParallel,
	}
	if _, rollbackErr := w.Store.Update(rollback); rollbackErr != nil {
		return Project{}, fmt.Errorf("%s memory repo: %w; rollback project metadata: %v", memoryRepoMode, repoErr, rollbackErr)
	}
	return Project{}, fmt.Errorf("%s memory repo: %w", memoryRepoMode, repoErr)
}
