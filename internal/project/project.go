// Package project defines the project model and store contract.
package project

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const GlobalSlug = "global"

var (
	ErrNotFound      = errors.New("project: not found")
	ErrAlreadyExists = errors.New("project: already exists")
	ErrInvalidSlug   = errors.New("project: invalid slug")
	ErrReservedSlug  = errors.New("project: slug is reserved")
	ErrDisplayName   = errors.New("project: display_name is required")
	ErrInvalidPath   = errors.New("project: directory path must be absolute")
	ErrMemoryRepo    = errors.New("project: memory repo path must be absolute")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Project is one row from the projects table.
type Project struct {
	Slug           string
	DisplayName    string
	MemoryRepoPath string
	ModelBinary    *string
	ModelPath      *string
	ModelCtxSize   *int
	ModelGPULayers *int
	ModelNParallel *int
	Hidden         bool
	CreatedAt      time.Time
	SavedAt        *time.Time
}

// Directory is one configured git directory for a project.
type Directory struct {
	ProjectSlug string
	Path        string
}

// CreateInput describes a user-created project. The global project is seeded
// by the database layer and cannot be created through the store.
type CreateInput struct {
	Slug           string
	DisplayName    string
	MemoryRepoPath string
	ModelBinary    *string
	ModelPath      *string
	ModelCtxSize   *int
	ModelGPULayers *int
	ModelNParallel *int
	Directories    []string
}

// UpdateInput edits mutable project fields. Slug is immutable.
type UpdateInput struct {
	Slug           string
	DisplayName    string
	MemoryRepoPath string
	ModelBinary    *string
	ModelPath      *string
	ModelCtxSize   *int
	ModelGPULayers *int
	ModelNParallel *int
}

// Store is the typed project persistence surface. SQL lives in internal/db.
type Store interface {
	List(includeHidden bool) ([]Project, error)
	Get(slug string) (Project, error)
	Create(input CreateInput) (Project, error)
	Update(input UpdateInput) (Project, error)
	SetHidden(slug string, hidden bool) error
	ListDirectories(slug string) ([]Directory, error)
}

func ValidateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: %q", ErrInvalidSlug, slug)
	}
	return nil
}

func ValidateCreatableSlug(slug string) error {
	slug = strings.TrimSpace(slug)
	if err := ValidateSlug(slug); err != nil {
		return err
	}
	if slug == GlobalSlug {
		return ErrReservedSlug
	}
	return nil
}

func ValidateDisplayName(displayName string) error {
	if strings.TrimSpace(displayName) == "" {
		return ErrDisplayName
	}
	return nil
}
