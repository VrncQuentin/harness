package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"

	gogit "github.com/go-git/go-git/v5"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/ui"
)

// OpenDB opens harness.db (running migrations + seed) and returns the handle
// plus the typed sub-stores. Any failure is surfaced to the UI as a startup
// error; the returned handle and stores may be nil, which callers must handle.
func OpenDB(uiServer *ui.Server, path string) (*db.DB, config.Store, metrics.Store) {
	d, err := db.Open(path)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("harness.db: %w", err))
		return nil, nil, nil
	}
	uiServer.SetConfigStore(d.Config())
	return d, d.Config(), d.Metrics()
}

// EnsureProjectMemoryRepo initializes and scaffolds one layout-v2 project
// memory repo. Existing git directories are opened as-is; non-git directories
// are initialized through go-git.
func EnsureProjectMemoryRepo(uiServer *ui.Server, store project.Store, slug string) bool {
	if store == nil {
		uiServer.AddStartupError(errors.New("project store unavailable"))
		return false
	}
	proj, err := store.Get(slug)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("project %s: %w", slug, err))
		return false
	}
	repo, err := gitw.Init(proj.MemoryRepoPath)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("project memory repo %s: %w", slug, err))
		return false
	}
	global := slug == project.GlobalSlug
	if err := memory.CreateMissingProjectRepo(proj.MemoryRepoPath, global); err != nil {
		uiServer.AddStartupError(fmt.Errorf("project memory repo layout %s: %w", slug, err))
		return false
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), projectRepoScaffoldFiles(global)); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
		slog.Warn("project memory repo scaffold commit", "project", slug, "err", err)
	}
	return true
}

func projectRepoScaffoldFiles(global bool) []string {
	items := memory.ExpectedProjectRepoLayout(global)
	files := make([]string, 0, len(items))
	for _, item := range items {
		if item.Dir {
			files = append(files, path.Join(item.Path, ".gitkeep"))
			continue
		}
		files = append(files, item.Path)
	}
	return files
}

// ValidatePaths checks startup-critical paths referenced by cfg and surfaces
// invalid entries as startup errors. It returns true when all checks passed.
func ValidatePaths(uiServer *ui.Server, cfg *config.Config) bool {
	ok := true
	checks := []struct {
		label string
		path  string
	}{
		{"model file", cfg.Model.ModelPath},
		{"llama-server binary", cfg.Model.Binary},
		{"embedder binary", cfg.Embedder.Binary},
		{"embedder model file", cfg.Embedder.ModelPath},
	}
	for _, c := range checks {
		if c.path == "" {
			continue
		}
		if err := validateFilePath(c.label, c.path); err != nil {
			uiServer.AddStartupError(err)
			ok = false
		}
	}
	return ok
}

func validateFilePath(label, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s not found: %s", label, path)
		}
		return fmt.Errorf("%s cannot be accessed: %s: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s points to a directory, expected a file: %s", label, path)
	}
	return nil
}
