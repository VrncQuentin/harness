package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VrncQuentin/harness/internal/project"
	gogit "github.com/go-git/go-git/v6"
)

type projectDirectoryStoreStub struct {
	dirs []project.Directory
	err  error
	slug string
}

func (s *projectDirectoryStoreStub) ListDirectories(slug string) ([]project.Directory, error) {
	s.slug = slug
	return s.dirs, s.err
}

func TestCheckProjectDirectories(t *testing.T) {
	validRepo := initGitRepo(t)
	nonGitDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "missing")
	filePath := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := &projectDirectoryStoreStub{dirs: []project.Directory{
		{ProjectSlug: "dt", Path: validRepo},
		{ProjectSlug: "dt", Path: nonGitDir},
		{ProjectSlug: "dt", Path: missingDir},
		{ProjectSlug: "dt", Path: filePath},
	}}
	warnings, err := CheckProjectDirectories(store, "dt")
	if err != nil {
		t.Fatalf("CheckProjectDirectories: %v", err)
	}
	if store.slug != "dt" {
		t.Fatalf("ListDirectories slug = %q, want dt", store.slug)
	}

	want := map[string]string{
		nonGitDir:  "not a git repository",
		missingDir: "directory missing",
		filePath:   "not a directory",
	}
	if len(warnings) != len(want) {
		t.Fatalf("warnings = %+v, want %d", warnings, len(want))
	}
	for _, warning := range warnings {
		if got := warning.Problem; got != want[warning.Path] {
			t.Errorf("warning for %s = %q, want %q", warning.Path, got, want[warning.Path])
		}
		delete(want, warning.Path)
	}
	for path, problem := range want {
		t.Errorf("missing warning for %s: %s", path, problem)
	}
}

func TestCheckProjectDirectoriesStoreError(t *testing.T) {
	sentinel := errors.New("list failed")
	_, err := CheckProjectDirectories(&projectDirectoryStoreStub{err: sentinel}, "dt")
	if !errors.Is(err, sentinel) {
		t.Fatalf("CheckProjectDirectories error = %v, want sentinel", err)
	}
}

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	return dir
}
