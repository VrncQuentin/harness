package project

import (
	"errors"
	"fmt"
)

const (
	MemoryRepoModeFresh = "fresh"
	MemoryRepoModeMove  = "move"
)

// RepoIdentity is a handle-bound proof that a project memory repository path
// identified a particular physical repository when the proof was taken. The
// runtime settles a project edit's repository identity through a pinned proof
// and re-verifies the boundary with it immediately before mutating, so a
// repointed alias between the decision and the mutation fails closed instead
// of authorizing a mutation of a changed repository.
type RepoIdentity interface {
	// SameAs reports whether path still identifies the physical repository the
	// proof was pinned to. It fails closed: an unresolvable path or a different
	// physical repository reports false.
	SameAs(path string) (bool, error)
	Close() error
}

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
	// PinRepoIdentity opens path and returns a handle-bound proof of the
	// physical repository it pinned. The caller retains the proof, re-verifies
	// the boundary with it at mutation time, and closes it.
	PinRepoIdentity(path string) (RepoIdentity, error)
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

// SettledUpdate carries a settled repository-identity decision for a project
// edit, backed by a handle-bound proof so the decision can be re-verified at
// the moment of mutation. The decision is private: a SettledUpdate is produced
// only by SettleUpdate (which marks it valid and, for a "same" decision, pins
// the destination), so a caller cannot forge one to bypass identity
// verification.
type SettledUpdate struct {
	valid    bool
	sameRepo bool
	proof    RepoIdentity
}

// IsSameRepo reports whether the settled destination identifies the same
// physical repository as the stored project's current one.
func (s SettledUpdate) IsSameRepo() bool { return s.sameRepo }

// SettleUpdate resolves whether input's destination memory repo is the same
// physical repository as the stored project's current one, and pins the
// destination with a handle-bound proof so the decision can be re-verified at
// mutation time. A failure to answer "is this the same repository" aborts the
// edit rather than folding into "different" (which would run a move against a
// repository that might be the destination itself) or "same" (which would
// silently drop a repointing the user asked for).
func (w *Workflow) SettleUpdate(input UpdateInput) (SettledUpdate, error) {
	if w.Store == nil {
		return SettledUpdate{}, errors.New("project: store not configured")
	}
	if w.Repos == nil {
		return SettledUpdate{}, errors.New("project: memory repo manager not configured")
	}
	current, err := w.Store.Get(input.Slug)
	if err != nil {
		return SettledUpdate{}, err
	}
	same, err := w.sameRepo(current, input)
	if err != nil {
		return SettledUpdate{}, err
	}
	var proof RepoIdentity
	if same && input.MemoryRepoPath != "" {
		proof, err = w.Repos.PinRepoIdentity(input.MemoryRepoPath)
		if err != nil {
			return SettledUpdate{}, err
		}
	}
	return SettledUpdate{valid: true, sameRepo: same, proof: proof}, nil
}

// ApplyUpdate applies an update using a settled decision from SettleUpdate. It
// re-verifies the destination boundary with the retained handle-bound proof
// immediately before mutating: an alias repointed after the decision fails
// closed instead of authorizing a mutation of a changed repository. Only a
// valid settlement produced by SettleUpdate is accepted; a forged or zero
// SettledUpdate is rejected.
func (w *Workflow) ApplyUpdate(input UpdateInput, memoryRepoMode string, settled SettledUpdate) (Project, error) {
	if w.Store == nil {
		return Project{}, errors.New("project: store not configured")
	}
	if w.Repos == nil {
		return Project{}, errors.New("project: memory repo manager not configured")
	}
	if !settled.valid {
		return Project{}, errors.New("project: invalid settled update (must come from SettleUpdate)")
	}
	if settled.proof != nil {
		defer func() { _ = settled.proof.Close() }()
		still, err := settled.proof.SameAs(input.MemoryRepoPath)
		if err != nil {
			return Project{}, fmt.Errorf("re-verify memory repo boundary: %w", err)
		}
		if !still {
			return Project{}, errors.New("memory repo boundary changed since the edit was checked")
		}
	}
	return w.updateResolved(input, memoryRepoMode, settled.sameRepo)
}

// Update persists project metadata and reconciles memory repo path changes with
// rollback when repo initialization or migration fails. It settles the
// repository identity once (SettleUpdate) and applies it with the handle-bound
// re-verification (ApplyUpdate).
func (w *Workflow) Update(input UpdateInput, memoryRepoMode string) (Project, error) {
	settled, err := w.SettleUpdate(input)
	if err != nil {
		return Project{}, err
	}
	return w.ApplyUpdate(input, memoryRepoMode, settled)
}

func (w *Workflow) sameRepo(current Project, input UpdateInput) (bool, error) {
	if input.MemoryRepoPath == "" || current.MemoryRepoPath == "" {
		return true, nil
	}
	return w.Repos.SameProjectRepoPath(input.MemoryRepoPath, current.MemoryRepoPath)
}

func (w *Workflow) updateResolved(input UpdateInput, memoryRepoMode string, sameRepo bool) (Project, error) {
	current, err := w.Store.Get(input.Slug)
	if err != nil {
		return Project{}, err
	}

	// Identity is settled before the metadata update, not after it. Resolving
	// later would leave a failure to answer "is this the same repository"
	// stranded between a committed metadata change and an untouched repo.
	memoryRepoChanged := !sameRepo
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
