package db

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vrnc/harness/internal/project"
)

func TestProjectStore_ListStartsWithGlobalAndFiltersHidden(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()

	if _, err := store.Create(project.CreateInput{Slug: "beta", DisplayName: "Beta"}); err != nil {
		t.Fatalf("create beta: %v", err)
	}
	if _, err := store.Create(project.CreateInput{Slug: "alpha", DisplayName: "Alpha"}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := store.SetHidden("beta", true); err != nil {
		t.Fatalf("hide beta: %v", err)
	}

	visible, err := store.List(false)
	if err != nil {
		t.Fatalf("List(false): %v", err)
	}
	if got := projectSlugs(visible); !reflect.DeepEqual(got, []string{"global", "alpha"}) {
		t.Fatalf("visible slugs = %v, want [global alpha]", got)
	}

	all, err := store.List(true)
	if err != nil {
		t.Fatalf("List(true): %v", err)
	}
	if got := projectSlugs(all); !reflect.DeepEqual(got, []string{"global", "alpha", "beta"}) {
		t.Fatalf("all slugs = %v, want [global alpha beta]", got)
	}
}

func TestProjectStore_CreateValidatesAndRoundTrips(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()

	modelPath := "C:/models/dt.gguf"
	ctxSize := 65536
	dir := filepath.Join(t.TempDir(), "repo")
	created, err := store.Create(project.CreateInput{
		Slug:         "dt-project",
		DisplayName:  "DT Project",
		ModelPath:    &modelPath,
		ModelCtxSize: &ctxSize,
		Directories:  []string{dir},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Slug != "dt-project" || created.DisplayName != "DT Project" {
		t.Fatalf("created project = %+v", created)
	}
	if created.ModelPath == nil || *created.ModelPath != modelPath {
		t.Fatalf("ModelPath = %v, want %q", created.ModelPath, modelPath)
	}
	if created.ModelCtxSize == nil || *created.ModelCtxSize != ctxSize {
		t.Fatalf("ModelCtxSize = %v, want %d", created.ModelCtxSize, ctxSize)
	}
	if created.SavedAt == nil {
		t.Fatal("SavedAt must be set on create")
	}
	if created.MemoryRepoPath == "" {
		t.Fatal("MemoryRepoPath must be set on create")
	}
	dirs, err := store.ListDirectories("dt-project")
	if err != nil {
		t.Fatalf("ListDirectories: %v", err)
	}
	if len(dirs) != 1 || dirs[0].Path != dir {
		t.Fatalf("directories = %+v, want %q", dirs, dir)
	}
}

func TestProjectStore_CreateRejectsInvalidInputs(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()

	tests := []struct {
		name  string
		input project.CreateInput
		want  error
	}{
		{"reserved global", project.CreateInput{Slug: "global", DisplayName: "Global"}, project.ErrReservedSlug},
		{"invalid slug", project.CreateInput{Slug: "Bad", DisplayName: "Bad"}, project.ErrInvalidSlug},
		{"empty display", project.CreateInput{Slug: "ok"}, project.ErrDisplayName},
		{"relative directory", project.CreateInput{Slug: "ok", DisplayName: "OK", Directories: []string{"relative"}}, project.ErrInvalidPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Create(tc.input)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Create: errors.Is(%v)=false, err=%v", tc.want, err)
			}
		})
	}
}

func TestProjectStore_CreateRejectsDuplicate(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()
	if _, err := store.Create(project.CreateInput{Slug: "dt", DisplayName: "DT"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := store.Create(project.CreateInput{Slug: "dt", DisplayName: "DT Again"}); !errors.Is(err, project.ErrAlreadyExists) {
		t.Fatalf("duplicate Create: errors.Is(ErrAlreadyExists)=false, err=%v", err)
	}
}

func TestProjectStore_DeleteRemovesProjectAndDirectories(t *testing.T) {
	store := newTestDB(t).Projects()
	if _, err := store.Create(project.CreateInput{
		Slug:           "demo",
		DisplayName:    "Demo",
		MemoryRepoPath: t.TempDir(),
		Directories:    []string{t.TempDir()},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete("demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get("demo"); !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrNotFound", err)
	}
	dirs, err := store.ListDirectories("demo")
	if !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("ListDirectories after Delete = %v, %v; want ErrNotFound", dirs, err)
	}
}
func TestProjectStore_UpdateMutableFields(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()
	if _, err := store.Create(project.CreateInput{Slug: "dt", DisplayName: "DT"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	modelBinary := "C:/bin/llama-server.exe"
	gpuLayers := 42
	updated, err := store.Update(project.UpdateInput{
		Slug:           "dt",
		DisplayName:    "DT Updated",
		ModelBinary:    &modelBinary,
		ModelGPULayers: &gpuLayers,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.DisplayName != "DT Updated" {
		t.Fatalf("DisplayName = %q", updated.DisplayName)
	}
	if updated.ModelBinary == nil || *updated.ModelBinary != modelBinary {
		t.Fatalf("ModelBinary = %v, want %q", updated.ModelBinary, modelBinary)
	}
	if updated.ModelGPULayers == nil || *updated.ModelGPULayers != gpuLayers {
		t.Fatalf("ModelGPULayers = %v, want %d", updated.ModelGPULayers, gpuLayers)
	}
	newRepo := filepath.Join(t.TempDir(), "memory")
	updated, err = store.Update(project.UpdateInput{
		Slug:           "dt",
		DisplayName:    "DT Updated",
		MemoryRepoPath: newRepo,
	})
	if err != nil {
		t.Fatalf("Update memory repo: %v", err)
	}
	if updated.MemoryRepoPath != newRepo {
		t.Fatalf("MemoryRepoPath = %q, want %q", updated.MemoryRepoPath, newRepo)
	}
}

func TestProjectStore_UpdateGlobalDisplayNameAllowed(t *testing.T) {
	d := newTestDB(t)
	updated, err := d.Projects().Update(project.UpdateInput{Slug: project.GlobalSlug, DisplayName: "Global Updated"})
	if err != nil {
		t.Fatalf("Update global display name: %v", err)
	}
	if updated.DisplayName != "Global Updated" {
		t.Fatalf("DisplayName = %q", updated.DisplayName)
	}
}

func TestProjectStore_UpdateMissingProject(t *testing.T) {
	d := newTestDB(t)
	_, err := d.Projects().Update(project.UpdateInput{Slug: "missing", DisplayName: "Missing"})
	if !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("Update missing: errors.Is(ErrNotFound)=false, err=%v", err)
	}
}

func TestProjectStore_HideAndUnhide(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()
	if _, err := store.Create(project.CreateInput{Slug: "dt", DisplayName: "DT"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetHidden("dt", true); err != nil {
		t.Fatalf("hide dt: %v", err)
	}
	p, err := store.Get("dt")
	if err != nil {
		t.Fatalf("Get dt: %v", err)
	}
	if !p.Hidden {
		t.Fatal("dt should be hidden")
	}
	if err := store.SetHidden("dt", false); err != nil {
		t.Fatalf("unhide dt: %v", err)
	}
	p, err = store.Get("dt")
	if err != nil {
		t.Fatalf("Get dt after unhide: %v", err)
	}
	if p.Hidden {
		t.Fatal("dt should be visible")
	}
}

func TestProjectStore_HideGlobalRejected(t *testing.T) {
	d := newTestDB(t)
	if err := d.Projects().SetHidden(project.GlobalSlug, true); err == nil {
		t.Fatal("expected error hiding global project, got nil")
	}
}

func TestProjectStore_Directories(t *testing.T) {
	d := newTestDB(t)
	store := d.Projects()
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if _, err := store.Create(project.CreateInput{Slug: "dt", DisplayName: "DT", Directories: []string{b, a}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	dirs, err := store.ListDirectories("dt")
	if err != nil {
		t.Fatalf("ListDirectories: %v", err)
	}
	if got := directoryPaths(dirs); !reflect.DeepEqual(got, []string{a, b}) {
		t.Fatalf("directories = %v, want [%s %s]", got, a, b)
	}
}

func TestProjectStore_ListDirectoriesRejectsMissingProject(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.Projects().ListDirectories("missing"); !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("ListDirectories missing project: errors.Is(ErrNotFound)=false, err=%v", err)
	}
}
func TestProjectStore_GetMissing(t *testing.T) {
	d := newTestDB(t)
	_, err := d.Projects().Get("missing")
	if !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("Get missing: errors.Is(ErrNotFound)=false, err=%v", err)
	}
}

func TestProjectStore_GetRejectsSpacedSlug(t *testing.T) {
	d := newTestDB(t)
	_, err := d.Projects().Get(" global ")
	if !errors.Is(err, project.ErrInvalidSlug) {
		t.Fatalf("Get spaced slug: errors.Is(ErrInvalidSlug)=false, err=%v", err)
	}
}

func projectSlugs(projects []project.Project) []string {
	out := make([]string, len(projects))
	for i, p := range projects {
		out[i] = p.Slug
	}
	return out
}

func directoryPaths(dirs []project.Directory) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}
