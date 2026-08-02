package runtime

import (
	"fmt"

	"github.com/VrncQuentin/harness/internal/project"
)

// resolveProject looks up a project by slug from the project store.
// Returns nil, error when the store is unavailable or the project is
// not found.
func (rt *Runtime) resolveProject(slug string) (*project.Project, error) {
	if slug == "" {
		slug = project.GlobalSlug
	}
	if rt.projectStore == nil {
		return nil, fmt.Errorf("project store not available")
	}
	proj, err := rt.projectStore.Get(slug)
	if err != nil {
		return nil, fmt.Errorf("resolve project %q: %w", slug, err)
	}
	return &proj, nil
}
