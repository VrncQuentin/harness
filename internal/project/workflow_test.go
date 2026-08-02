package project

import (
	"errors"
	"testing"
)

type workflowStore struct {
	projects map[string]Project
	deleted  []string
}

func newWorkflowStore(projects ...Project) *workflowStore {
	s := &workflowStore{projects: map[string]Project{}}
	for _, p := range projects {
		s.projects[p.Slug] = p
	}
	return s
}

func (s *workflowStore) List(bool) ([]Project, error) { return nil, nil }
func (s *workflowStore) Get(slug string) (Project, error) {
	p, ok := s.projects[slug]
	if !ok {
		return Project{}, ErrNotFound
	}
	return p, nil
}
func (s *workflowStore) Create(input CreateInput) (Project, error) {
	p := Project{Slug: input.Slug, DisplayName: input.DisplayName, MemoryRepoPath: input.MemoryRepoPath}
	if p.MemoryRepoPath == "" {
		p.MemoryRepoPath = "/default/" + p.Slug
	}
	s.projects[p.Slug] = p
	return p, nil
}
func (s *workflowStore) Update(input UpdateInput) (Project, error) {
	p, ok := s.projects[input.Slug]
	if !ok {
		return Project{}, ErrNotFound
	}
	p.DisplayName = input.DisplayName
	p.MemoryRepoPath = input.MemoryRepoPath
	p.ModelBinary = input.ModelBinary
	p.ModelPath = input.ModelPath
	p.ModelCtxSize = input.ModelCtxSize
	p.ModelGPULayers = input.ModelGPULayers
	p.ModelNParallel = input.ModelNParallel
	s.projects[p.Slug] = p
	return p, nil
}
func (s *workflowStore) Delete(slug string) error {
	delete(s.projects, slug)
	s.deleted = append(s.deleted, slug)
	return nil
}
func (s *workflowStore) SetHidden(string, bool) error                { return nil }
func (s *workflowStore) ListDirectories(string) ([]Directory, error) { return nil, nil }

type workflowRepos struct {
	ensureErr error
	moveErr   error
	sameErr   error
	ensures   []string
	moves     [][2]string
	// sameCalls counts SameProjectRepoPath invocations. Tests use it to prove
	// a settled identity decision is carried through without recomputation.
	sameCalls int
}

func (r *workflowRepos) EnsureProjectRepo(root string, global bool) error {
	r.ensures = append(r.ensures, root)
	return r.ensureErr
}
func (r *workflowRepos) MoveProjectRepo(src, dst string, global bool) error {
	r.moves = append(r.moves, [2]string{src, dst})
	return r.moveErr
}
func (r *workflowRepos) SameProjectRepoPath(a, b string) (bool, error) {
	r.sameCalls++
	if r.sameErr != nil {
		return false, r.sameErr
	}
	return a == b, nil
}

func TestWorkflowCreateRollsBackProjectWhenRepoInitFails(t *testing.T) {
	store := newWorkflowStore()
	repos := &workflowRepos{ensureErr: errors.New("disk full")}
	workflow := NewWorkflow(store, repos)

	_, err := workflow.Create(CreateInput{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/demo"})
	if err == nil || !errors.Is(err, repos.ensureErr) {
		t.Fatalf("Create error = %v, want repo error", err)
	}
	if _, ok := store.projects["demo"]; ok {
		t.Fatal("project metadata was not rolled back")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "demo" {
		t.Fatalf("deleted = %v, want [demo]", store.deleted)
	}
}

func TestWorkflowUpdateRollsBackMetadataWhenRepoInitFails(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/old"})
	repos := &workflowRepos{ensureErr: errors.New("cannot init")}
	workflow := NewWorkflow(store, repos)

	_, err := workflow.Update(UpdateInput{Slug: "demo", DisplayName: "Renamed", MemoryRepoPath: "/repo/new"}, MemoryRepoModeFresh)
	if err == nil || !errors.Is(err, repos.ensureErr) {
		t.Fatalf("Update error = %v, want repo error", err)
	}
	got := store.projects["demo"]
	if got.DisplayName != "Demo" || got.MemoryRepoPath != "/repo/old" {
		t.Fatalf("metadata after rollback = %+v", got)
	}
}

func TestWorkflowUpdateRejectsMissingMoveModeBeforeMetadataChange(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/old"})
	repos := &workflowRepos{}
	workflow := NewWorkflow(store, repos)

	_, err := workflow.Update(UpdateInput{Slug: "demo", DisplayName: "Renamed", MemoryRepoPath: "/repo/new"}, "")
	if err == nil {
		t.Fatal("expected missing mode error")
	}
	got := store.projects["demo"]
	if got.DisplayName != "Demo" || got.MemoryRepoPath != "/repo/old" {
		t.Fatalf("metadata changed before mode validation: %+v", got)
	}
	if len(repos.ensures) != 0 || len(repos.moves) != 0 {
		t.Fatalf("repo operations ran before mode validation: ensures=%v moves=%v", repos.ensures, repos.moves)
	}
}

// TestWorkflowUpdateResolvedCarriesSettledIdentity verifies that a caller that
// settled the repository-identity decision once can carry it into the mutation:
// UpdateResolved never recomputes the identity and never runs a move when the
// settled decision is "same".
func TestWorkflowUpdateResolvedCarriesSettledIdentity(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/old"})
	repos := &workflowRepos{}
	workflow := NewWorkflow(store, repos)

	updated, err := workflow.UpdateResolved(UpdateInput{
		Slug: "demo", DisplayName: "Renamed", MemoryRepoPath: "/alias",
	}, MemoryRepoModeMove, true)
	if err != nil {
		t.Fatalf("UpdateResolved: %v", err)
	}
	if updated.MemoryRepoPath != "/alias" {
		t.Fatalf("memory repo path = %q, want the alias", updated.MemoryRepoPath)
	}
	if len(repos.moves) != 0 {
		t.Fatalf("a move ran despite the settled 'same' decision: %v", repos.moves)
	}
	if repos.sameCalls != 0 {
		t.Fatalf("SameProjectRepoPath called %d times; the settled decision must not be recomputed", repos.sameCalls)
	}
}

// TestWorkflowUpdateResolvedAppliesSettledMove verifies that a settled
// "different" decision does execute the move through UpdateResolved.
func TestWorkflowUpdateResolvedAppliesSettledMove(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/old"})
	repos := &workflowRepos{}
	workflow := NewWorkflow(store, repos)

	updated, err := workflow.UpdateResolved(UpdateInput{
		Slug: "demo", DisplayName: "Renamed", MemoryRepoPath: "/repo/new",
	}, MemoryRepoModeMove, false)
	if err != nil {
		t.Fatalf("UpdateResolved: %v", err)
	}
	if updated.MemoryRepoPath != "/repo/new" {
		t.Fatalf("memory repo path = %q, want /repo/new", updated.MemoryRepoPath)
	}
	if len(repos.moves) != 1 || repos.moves[0] != [2]string{"/repo/old", "/repo/new"} {
		t.Fatalf("moves = %v, want the settled move", repos.moves)
	}
}

// An identity that cannot be resolved has to abort the whole update. Folding it
// into "the paths differ" would run a move against a repository that might be
// the destination itself; folding it into "the paths match" would silently drop
// a repointing the user asked for. Either way the metadata must not move.
func TestWorkflowUpdateAbortsWhenRepoIdentityCannotBeResolved(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "demo", DisplayName: "Demo", MemoryRepoPath: "/repo/old"})
	repos := &workflowRepos{sameErr: errors.New("cannot resolve")}
	workflow := NewWorkflow(store, repos)

	_, err := workflow.Update(UpdateInput{Slug: "demo", DisplayName: "Renamed", MemoryRepoPath: "/repo/new"}, MemoryRepoModeMove)
	if !errors.Is(err, repos.sameErr) {
		t.Fatalf("Update error = %v, want the identity error", err)
	}
	got := store.projects["demo"]
	if got.DisplayName != "Demo" || got.MemoryRepoPath != "/repo/old" {
		t.Fatalf("metadata changed despite an unresolved repo identity: %+v", got)
	}
	if len(repos.ensures) != 0 || len(repos.moves) != 0 {
		t.Fatalf("repo operations ran on an unresolved identity: ensures=%v moves=%v", repos.ensures, repos.moves)
	}
}
