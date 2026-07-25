package ui

import "github.com/VrncQuentin/harness/internal/project"

// ProjectStore is the subset of project.Store the UI handlers need.
type ProjectStore interface {
	List(includeHidden bool) ([]project.Project, error)
	Get(slug string) (project.Project, error)
	Create(input project.CreateInput) (project.Project, error)
	Update(input project.UpdateInput) (project.Project, error)
	SetHidden(slug string, hidden bool) error
	ListDirectories(slug string) ([]project.Directory, error)
}
