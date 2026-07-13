// Package db owns every piece of SQL in the harness: the SQLite driver
// registration, connection opening, migration runner, and all CRUD
// statements used by other subsystems. No other package imports
// database/sql or a SQL driver.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite" // register the sqlite driver

	"github.com/vrnc/harness/migrations"
)

// DB owns the shared harness SQLite handle and exposes typed stores for each
// subsystem. Callers open it once in main and pass the sub-stores around.
type DB struct {
	sqldb    *sql.DB
	cfg      *ConfigStore
	metrics  *MetricsStore
	projects *ProjectStore
}

// Open opens harness.db at path, applies any pending migrations, and seeds
// the singleton config row. The returned *DB must be closed via Close.
func Open(path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", foreignKeysDSN(path))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}

	if err := runMigrations(sqldb); err != nil {
		_ = sqldb.Close()
		return nil, err
	}

	harnessHome := filepath.Dir(path)
	d := &DB{sqldb: sqldb}
	d.cfg = &ConfigStore{db: sqldb}
	d.metrics = &MetricsStore{db: sqldb}
	d.projects = &ProjectStore{db: sqldb, harnessHome: harnessHome}

	if err := d.seedGlobalProject(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	if err := d.cfg.seed(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return d, nil
}

// PeekUIPort reads the saved UI port from an existing harness.db without
// running migrations or seeding defaults. It is intentionally best-effort:
// startup still opens the real DB after the UI is serving, where any errors
// are surfaced in the browser.
func PeekUIPort(path string, fallback int) int {
	if _, err := os.Stat(path); err != nil {
		return fallback
	}

	sqldb, err := sql.Open("sqlite", foreignKeysDSN(path))
	if err != nil {
		return fallback
	}
	defer func() { _ = sqldb.Close() }()

	var port int
	if err := sqldb.QueryRow(`SELECT ui_port FROM config WHERE id = 1`).Scan(&port); err != nil {
		return fallback
	}
	if port < 1 || port > 65535 {
		return fallback
	}
	return port
}

func foreignKeysDSN(path string) string {
	if strings.Contains(path, "?_pragma=") || strings.Contains(path, "&_pragma=") {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&_pragma=foreign_keys(1)"
	}
	return path + "?_pragma=foreign_keys(1)"
}

// Close closes the underlying SQLite handle.
func (d *DB) Close() error {
	return d.sqldb.Close()
}

// Config returns the config sub-store.
func (d *DB) Config() *ConfigStore { return d.cfg }

// Metrics returns the metrics sub-store.
func (d *DB) Metrics() *MetricsStore { return d.metrics }

// Projects returns the project metadata sub-store.
func (d *DB) Projects() *ProjectStore { return d.projects }

func (d *DB) seedGlobalProject() error {
	_, err := d.sqldb.Exec(
		`INSERT OR IGNORE INTO projects (slug, display_name, memory_repo_path, hidden, created_at) VALUES (?, ?, ?, ?, ?)`,
		"global", "Global", d.projects.defaultMemoryRepoPath("global"), 0, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: seed global project: %w", err)
	}
	_, err = d.sqldb.Exec(`UPDATE projects SET memory_repo_path = ? WHERE slug = ? AND (memory_repo_path IS NULL OR memory_repo_path = '')`, d.projects.defaultMemoryRepoPath("global"), "global")
	if err != nil {
		return fmt.Errorf("db: update global project memory repo: %w", err)
	}
	return nil
}

// runMigrations applies every pending migration from the embedded FS using
// golang-migrate. Running against a DB that is already up-to-date is a no-op.
func runMigrations(sqldb *sql.DB) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("db: migrations source: %w", err)
	}
	driver, err := migratesqlite.WithInstance(sqldb, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("db: migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("db: migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}
