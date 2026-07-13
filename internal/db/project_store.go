package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vrnc/harness/internal/home"
	"github.com/vrnc/harness/internal/project"
)

// ProjectStore persists M3b project metadata and attached directories.
type ProjectStore struct {
	db          *sql.DB
	harnessHome string
}

var _ project.Store = (*ProjectStore)(nil)

func (s *ProjectStore) List(includeHidden bool) ([]project.Project, error) {
	query := `SELECT slug, display_name, memory_repo_path, model_binary, model_path, model_ctx_size,
		model_gpu_layers, model_n_parallel, hidden, created_at, saved_at
		FROM projects`
	args := []any{}
	if !includeHidden {
		query += ` WHERE hidden = 0 OR slug = ?`
		args = append(args, project.GlobalSlug)
	}
	query += ` ORDER BY CASE WHEN slug = ? THEN 0 ELSE 1 END, display_name COLLATE NOCASE, slug`
	args = append(args, project.GlobalSlug)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.withDefaultMemoryRepoPath(p))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list projects rows: %w", err)
	}
	return out, nil
}

func (s *ProjectStore) Get(slug string) (project.Project, error) {
	if err := project.ValidateSlug(slug); err != nil {
		return project.Project{}, err
	}
	row := s.db.QueryRow(`SELECT slug, display_name, memory_repo_path, model_binary, model_path, model_ctx_size,
		model_gpu_layers, model_n_parallel, hidden, created_at, saved_at
		FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return project.Project{}, project.ErrNotFound
		}
		return project.Project{}, err
	}
	return s.withDefaultMemoryRepoPath(p), nil
}

func (s *ProjectStore) Create(input project.CreateInput) (project.Project, error) {
	input.Slug = strings.TrimSpace(input.Slug)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if err := project.ValidateCreatableSlug(input.Slug); err != nil {
		return project.Project{}, err
	}
	if err := project.ValidateDisplayName(input.DisplayName); err != nil {
		return project.Project{}, err
	}
	input.MemoryRepoPath = strings.TrimSpace(input.MemoryRepoPath)
	if input.MemoryRepoPath == "" {
		input.MemoryRepoPath = s.defaultMemoryRepoPath(input.Slug)
	}
	if err := validateMemoryRepoPath(input.MemoryRepoPath); err != nil {
		return project.Project{}, err
	}
	for _, dir := range input.Directories {
		if err := validateDirectoryPath(dir); err != nil {
			return project.Project{}, err
		}
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return project.Project{}, fmt.Errorf("db: begin create project: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`INSERT INTO projects (
		slug, display_name, memory_repo_path, model_binary, model_path, model_ctx_size,
		model_gpu_layers, model_n_parallel, hidden, created_at, saved_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		input.Slug, input.DisplayName, input.MemoryRepoPath, input.ModelBinary, input.ModelPath, input.ModelCtxSize,
		input.ModelGPULayers, input.ModelNParallel, now, now,
	)
	if err != nil {
		if isConstraintError(err) {
			return project.Project{}, project.ErrAlreadyExists
		}
		return project.Project{}, fmt.Errorf("db: create project %s: %w", input.Slug, err)
	}
	for _, dir := range input.Directories {
		if _, err := tx.Exec(`INSERT INTO project_directories(project_slug, path) VALUES(?, ?)`, input.Slug, dir); err != nil {
			return project.Project{}, fmt.Errorf("db: add project directory %s: %w", input.Slug, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return project.Project{}, fmt.Errorf("db: commit create project %s: %w", input.Slug, err)
	}
	return s.Get(input.Slug)
}

func (s *ProjectStore) Update(input project.UpdateInput) (project.Project, error) {
	input.Slug = strings.TrimSpace(input.Slug)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if err := project.ValidateSlug(input.Slug); err != nil {
		return project.Project{}, err
	}
	if err := project.ValidateDisplayName(input.DisplayName); err != nil {
		return project.Project{}, err
	}
	input.MemoryRepoPath = strings.TrimSpace(input.MemoryRepoPath)
	if input.MemoryRepoPath == "" {
		input.MemoryRepoPath = s.defaultMemoryRepoPath(input.Slug)
	}
	if err := validateMemoryRepoPath(input.MemoryRepoPath); err != nil {
		return project.Project{}, err
	}
	res, err := s.db.Exec(`UPDATE projects SET
		display_name = ?, memory_repo_path = ?, model_binary = ?, model_path = ?, model_ctx_size = ?,
		model_gpu_layers = ?, model_n_parallel = ?, saved_at = ?
		WHERE slug = ?`,
		input.DisplayName, input.MemoryRepoPath, input.ModelBinary, input.ModelPath, input.ModelCtxSize,
		input.ModelGPULayers, input.ModelNParallel, time.Now().Unix(), input.Slug,
	)
	if err != nil {
		return project.Project{}, fmt.Errorf("db: update project %s: %w", input.Slug, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return project.Project{}, fmt.Errorf("db: update project rows %s: %w", input.Slug, err)
	} else if n == 0 {
		return project.Project{}, project.ErrNotFound
	}
	return s.Get(input.Slug)
}

func (s *ProjectStore) SetHidden(slug string, hidden bool) error {
	if err := project.ValidateSlug(slug); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE projects SET hidden = ?, saved_at = ? WHERE slug = ?`, boolInt(hidden), time.Now().Unix(), slug)
	if err != nil {
		return fmt.Errorf("db: set project hidden %s: %w", slug, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("db: set project hidden rows %s: %w", slug, err)
	} else if n == 0 {
		return project.ErrNotFound
	}
	return nil
}

func (s *ProjectStore) ListDirectories(slug string) ([]project.Directory, error) {
	if err := project.ValidateSlug(slug); err != nil {
		return nil, err
	}
	if ok, err := s.projectExists(slug); err != nil {
		return nil, err
	} else if !ok {
		return nil, project.ErrNotFound
	}
	rows, err := s.db.Query(`SELECT project_slug, path FROM project_directories WHERE project_slug = ? ORDER BY path`, slug)
	if err != nil {
		return nil, fmt.Errorf("db: list project directories %s: %w", slug, err)
	}
	defer func() { _ = rows.Close() }()

	var out []project.Directory
	for rows.Next() {
		var d project.Directory
		if err := rows.Scan(&d.ProjectSlug, &d.Path); err != nil {
			return nil, fmt.Errorf("db: scan project directory: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list project directories rows: %w", err)
	}
	return out, nil
}

func (s *ProjectStore) AddDirectory(slug, path string) error {
	if err := project.ValidateSlug(slug); err != nil {
		return err
	}
	if ok, err := s.projectExists(slug); err != nil {
		return err
	} else if !ok {
		return project.ErrNotFound
	}
	if err := validateDirectoryPath(path); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO project_directories(project_slug, path) VALUES(?, ?)`, slug, path)
	if err != nil {
		return fmt.Errorf("db: add project directory %s: %w", slug, err)
	}
	return nil
}

func (s *ProjectStore) RemoveDirectory(slug, path string) error {
	if err := project.ValidateSlug(slug); err != nil {
		return err
	}
	if ok, err := s.projectExists(slug); err != nil {
		return err
	} else if !ok {
		return project.ErrNotFound
	}
	if err := validateDirectoryPath(path); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM project_directories WHERE project_slug = ? AND path = ?`, slug, path)
	if err != nil {
		return fmt.Errorf("db: remove project directory %s: %w", slug, err)
	}
	return nil
}

func (s *ProjectStore) projectExists(slug string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM projects WHERE slug = ?`, slug).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("db: check project exists %s: %w", slug, err)
	}
	return true, nil
}

type projectScanner interface {
	Scan(dest ...any) error
}

func scanProject(row projectScanner) (project.Project, error) {
	var (
		p              project.Project
		memoryRepoPath sql.NullString
		modelBinary    sql.NullString
		modelPath      sql.NullString
		modelCtxSize   sql.NullInt64
		modelGPULayers sql.NullInt64
		modelNParallel sql.NullInt64
		hidden         int
		createdAt      int64
		savedAt        sql.NullInt64
	)
	if err := row.Scan(
		&p.Slug, &p.DisplayName, &memoryRepoPath, &modelBinary, &modelPath, &modelCtxSize,
		&modelGPULayers, &modelNParallel, &hidden, &createdAt, &savedAt,
	); err != nil {
		return project.Project{}, fmt.Errorf("db: scan project: %w", err)
	}
	if memoryRepoPath.Valid {
		p.MemoryRepoPath = strings.TrimSpace(memoryRepoPath.String)
	}
	p.ModelBinary = stringPtr(modelBinary)
	p.ModelPath = stringPtr(modelPath)
	p.ModelCtxSize = intPtr(modelCtxSize)
	p.ModelGPULayers = intPtr(modelGPULayers)
	p.ModelNParallel = intPtr(modelNParallel)
	p.Hidden = hidden != 0
	p.CreatedAt = time.Unix(createdAt, 0)
	p.SavedAt = timePtr(savedAt)
	return p, nil
}

func (s *ProjectStore) withDefaultMemoryRepoPath(p project.Project) project.Project {
	if strings.TrimSpace(p.MemoryRepoPath) == "" {
		p.MemoryRepoPath = s.defaultMemoryRepoPath(p.Slug)
	}
	return p
}

func (s *ProjectStore) defaultMemoryRepoPath(slug string) string {
	root := s.harnessHome
	if root == "" {
		root = "."
	}
	repoPath, err := home.ProjectRepoPath(root, slug)
	if err != nil {
		return filepath.Join(root, "projects", slug)
	}
	return repoPath
}

func validateDirectoryPath(path string) error {
	if !isAbsPath(path) {
		return project.ErrInvalidPath
	}
	return nil
}

func validateMemoryRepoPath(path string) error {
	if !isAbsPath(path) {
		return project.ErrMemoryRepo
	}
	return nil
}

func isAbsPath(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func isConstraintError(err error) bool {
	return strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func stringPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func intPtr(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

func timePtr(ni sql.NullInt64) *time.Time {
	if !ni.Valid {
		return nil
	}
	v := time.Unix(ni.Int64, 0)
	return &v
}
