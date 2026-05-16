package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/ui"
)

type projectDirectoryStore interface {
	ListDirectories(slug string) ([]project.Directory, error)
}

// SetProjectStore wires the project metadata store used for advisory runtime
// health checks. A nil store disables project directory warnings.
func (rt *Runtime) SetProjectStore(store project.Store) {
	rt.mu.Lock()
	rt.projectStore = store
	rt.mu.Unlock()
}

// CheckProjectDirectories validates configured project directories without
// blocking activation. Store-level errors are returned; per-directory failures
// are accumulated as UI warnings.
func CheckProjectDirectories(store projectDirectoryStore, slug string) ([]ui.ProjectDirectoryWarning, error) {
	if store == nil {
		return nil, nil
	}
	dirs, err := store.ListDirectories(slug)
	if err != nil {
		return nil, fmt.Errorf("project directory health: %w", err)
	}

	warnings := make([]ui.ProjectDirectoryWarning, 0)
	for _, dir := range dirs {
		info, err := os.Stat(dir.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			warnings = append(warnings, ui.ProjectDirectoryWarning{Path: dir.Path, Problem: "directory missing"})
			continue
		case err != nil:
			warnings = append(warnings, ui.ProjectDirectoryWarning{Path: dir.Path, Problem: fmt.Sprintf("directory unreadable: %v", err)})
			continue
		case !info.IsDir():
			warnings = append(warnings, ui.ProjectDirectoryWarning{Path: dir.Path, Problem: "not a directory"})
			continue
		}

		if _, err := gitw.Open(dir.Path); err != nil {
			warnings = append(warnings, ui.ProjectDirectoryWarning{Path: dir.Path, Problem: "not a git repository"})
		}
	}
	return warnings, nil
}

func (rt *Runtime) refreshProjectDirectoryWarnings(uiServer *ui.Server) {
	if uiServer == nil {
		return
	}
	slug := rt.cfg.Project.ActiveProjectSlug
	if slug == "" {
		slug = project.GlobalSlug
	}
	warnings, err := CheckProjectDirectories(rt.projectStore, slug)
	if err != nil {
		slog.Warn("project directory health check failed", "project", slug, "err", err)
		uiServer.SetProjectDirectoryWarnings(slug, nil)
		return
	}
	uiServer.SetProjectDirectoryWarnings(slug, warnings)
}
