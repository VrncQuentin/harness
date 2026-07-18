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
func (r *workflowRepos) SameProjectRepoPath(a, b string) bool { return a == b }

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
