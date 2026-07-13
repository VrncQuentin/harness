package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/metrics"
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
	if cfg.Memory.RepoPath != "" {
		if err := memory.ValidateRepo(cfg.Memory.RepoPath); err != nil {
			uiServer.AddStartupError(fmt.Errorf("memory repo: %w", err))
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
