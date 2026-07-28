package project

import (
	"errors"
	"testing"
	"time"
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

// sequencedWorkflowRepos wraps workflowRepos so a test can hold
// EnsureProjectRepo in flight: it reports the root it was called with on
// entered (so the test can confirm which call reached it) and then blocks on
// release before delegating to the embedded stub.
type sequencedWorkflowRepos struct {
	*workflowRepos
	entered chan string
	release chan struct{}
}

func (r *sequencedWorkflowRepos) EnsureProjectRepo(root string, global bool) error {
	r.entered <- root
	<-r.release
	return r.workflowRepos.EnsureProjectRepo(root, global)
}

// Two callers reaching Workflow.Create/Update concurrently is a realistic
// scenario -- an HTTP handler builds a fresh *Workflow per request (see
// internal/ui/projects_page.go), so nothing about the request path itself
// serializes them -- and both could plausibly name the same MemoryRepoPath:
// a "create project" naming a path another project is about to move onto, or
// two edits racing on the same destination. Without workflowMu, both calls
// would reach EnsureProjectRepo/MoveProjectRepo concurrently and interleave
// their writes to the same destination directory.
//
// This proves the lock actually serializes them, not just that both calls
// eventually succeed: a Create is parked inside EnsureProjectRepo (holding
// workflowMu), and a concurrent Update naming the same destination must not
// reach its own EnsureProjectRepo call until the Create finishes and
// releases the lock.
func TestWorkflowSerializesConcurrentCreateAndUpdateOnTheSameDestination(t *testing.T) {
	store := newWorkflowStore(Project{Slug: "existing", DisplayName: "Existing", MemoryRepoPath: "/shared/repo"})
	repos := &sequencedWorkflowRepos{
		workflowRepos: &workflowRepos{},
		entered:       make(chan string, 1),
		release:       make(chan struct{}),
	}
	workflow := NewWorkflow(store, repos)

	createDone := make(chan error, 1)
	go func() {
		_, err := workflow.Create(CreateInput{Slug: "new", DisplayName: "New", MemoryRepoPath: "/shared/repo"})
		createDone <- err
	}()

	select {
	case root := <-repos.entered:
		if root != "/shared/repo" {
			t.Fatalf("EnsureProjectRepo root = %q, want /shared/repo", root)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create never reached EnsureProjectRepo")
	}

	updateDone := make(chan error, 1)
	go func() {
		_, err := workflow.Update(UpdateInput{Slug: "existing", DisplayName: "Existing", MemoryRepoPath: "/shared/repo/moved"}, MemoryRepoModeFresh)
		updateDone <- err
	}()

	select {
	case <-repos.entered:
		t.Fatal("Update reached EnsureProjectRepo while Create was still in flight -- workflowMu did not serialize them")
	case <-time.After(150 * time.Millisecond):
		// Still blocked on workflowMu, as it must be.
	}

	close(repos.release)

	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create did not finish after release")
	}
	select {
	case <-repos.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Update never reached EnsureProjectRepo after Create released the lock")
	}
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Update did not finish after Create released the lock")
	}
}
