package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
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

// ValidatePaths checks that the binaries and model files referenced by cfg
// exist on disk and surfaces any missing ones as startup errors.
func ValidatePaths(uiServer *ui.Server, cfg *config.Config) {
	checks := []struct {
		label, path string
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
		if _, err := os.Stat(c.path); errors.Is(err, fs.ErrNotExist) {
			uiServer.AddStartupError(fmt.Errorf("%s not found: %s", c.label, c.path))
		}
	}
}
