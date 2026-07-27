// Package db owns every piece of SQL in the harness: the SQLite driver
// registration, connection opening, migration runner, and all CRUD
// statements used by other subsystems. No other package imports
// database/sql or a SQL driver.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/migrations"
	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite" // register the sqlite driver
)

// DB owns the shared harness SQLite handle and exposes typed stores for each
// subsystem. Callers open it once in main and pass the sub-stores around.
type DB struct {
	sqldb    *sql.DB
	cfg      *ConfigStore
	metrics  *MetricsStore
	projects *ProjectStore
}

// DefaultMemoryRepoPathFunc returns the default project memory repo path for slug.
// The caller owns path policy; db only persists the resulting value.
type DefaultMemoryRepoPathFunc func(slug string) (string, error)

// Open opens harness.db at path, applies any pending migrations, and seeds
// the singleton config row. The returned *DB must be closed via Close.
func Open(path string, defaultMemoryRepoPath DefaultMemoryRepoPathFunc) (*DB, error) {
	if defaultMemoryRepoPath == nil {
		return nil, errors.New("db: default memory repo path function is required")
	}

	sqldb, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	configureSQLitePool(sqldb)
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}

	if err := runMigrations(sqldb); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	d := &DB{sqldb: sqldb}
	d.cfg = &ConfigStore{db: sqldb}
	d.metrics = &MetricsStore{db: sqldb}
	d.projects = &ProjectStore{
		db:                    sqldb,
		defaultMemoryRepoPath: defaultMemoryRepoPath,
	}

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
	// The database is opened by the SQLite driver, which takes a DSN string and
	// cannot be handed a file handle, so this path is a pathname either way.
	// The Stat only decides whether to attempt the open at all; a wrong answer
	// costs the fallback port, not access to anything. See the filesystem
	// access ledger in docs/architecture.md.
	if _, err := os.Stat(path); err != nil {
		return fallback
	}

	sqldb, err := sql.Open("sqlite", sqliteDSN(path))
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

const sqliteBusyTimeoutMS = 5000

var requiredSQLitePragmas = []string{
	"foreign_keys(1)",
	fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMS),
	"journal_mode(WAL)",
}

func configureSQLitePool(sqldb *sql.DB) {
	// harness.db receives concurrent writes from config, metrics, and project
	// stores. Keep one SQLite connection so writes are serialized deliberately;
	// busy_timeout still protects against external readers/writers holding locks.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
}

func sqliteDSN(path string) string {
	dsn := path
	for _, pragma := range requiredSQLitePragmas {
		name := sqlitePragmaName(pragma)
		if hasSQLitePragma(dsn, name) {
			continue
		}
		dsn = appendSQLitePragma(dsn, pragma)
	}
	return dsn
}

func appendSQLitePragma(dsn, pragma string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=" + url.QueryEscape(pragma)
}

func hasSQLitePragma(dsn, name string) bool {
	idx := strings.Index(dsn, "?")
	if idx < 0 || idx == len(dsn)-1 {
		return false
	}
	query := dsn[idx+1:]
	if hash := strings.Index(query, "#"); hash >= 0 {
		query = query[:hash]
	}
	for _, part := range strings.Split(query, "&") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key != "_pragma" {
			continue
		}
		decoded, err := url.QueryUnescape(value)
		if err != nil {
			decoded = value
		}
		if sqlitePragmaName(decoded) == name {
			return true
		}
	}
	return false
}

func sqlitePragmaName(pragma string) string {
	pragma = strings.TrimSpace(strings.ToLower(pragma))
	if name, _, ok := strings.Cut(pragma, "("); ok {
		return strings.TrimSpace(name)
	}
	if name, _, ok := strings.Cut(pragma, "="); ok {
		return strings.TrimSpace(name)
	}
	return pragma
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
	globalRepoPath, err := d.projects.defaultPathForSlug("global")
	if err != nil {
		return err
	}
	_, err = d.sqldb.Exec(
		`INSERT OR IGNORE INTO projects (slug, display_name, memory_repo_path, hidden, created_at) VALUES (?, ?, ?, ?, ?)`,
		"global", "Global", globalRepoPath, 0, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: seed global project: %w", err)
	}
	_, err = d.sqldb.Exec(`UPDATE projects SET memory_repo_path = ? WHERE slug = ? AND (memory_repo_path IS NULL OR memory_repo_path = '')`, globalRepoPath, "global")
	if err != nil {
		return fmt.Errorf("db: update global project memory repo: %w", err)
	}
	return nil
}

// runMigrations applies every pending migration from the embedded FS using
// golang-migrate. Running against a DB that is already up-to-date is a no-op.
func runMigrations(sqldb *sql.DB) error {
	return runMigrationsFS(sqldb, migrations.FS)
}

func runMigrationsFS(sqldb *sql.DB, fsys fs.FS) error {
	bundledVersion, err := bundledMigrationVersion(fsys, ".")
	if err != nil {
		return err
	}
	src, err := iofs.New(fsys, ".")
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
	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("db: read migration version: %w", err)
	}
	if dirty {
		return fmt.Errorf("db: migration version %d is dirty; delete harness.db and restart", version)
	}
	if err == nil && version > bundledVersion {
		return fmt.Errorf("db: migration version %d is newer than bundled schema version %d; delete harness.db and restart", version, bundledVersion)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	version, dirty, err = m.Version()
	if err != nil {
		return fmt.Errorf("db: read final migration version: %w", err)
	}
	if dirty {
		return fmt.Errorf("db: migration version %d is dirty after applying bundled schema; delete harness.db and restart", version)
	}
	if version != bundledVersion {
		return fmt.Errorf("db: migration ended at version %d, want bundled schema version %d; delete harness.db and restart", version, bundledVersion)
	}
	return nil
}
func bundledMigrationVersion(fsys fs.FS, dir string) (uint, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0, fmt.Errorf("db: read bundled migrations: %w", err)
	}
	var max uint
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		migration, err := source.Parse(entry.Name())
		if errors.Is(err, source.ErrParse) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("db: parse bundled migration %s: %w", entry.Name(), err)
		}
		if migration.Version > max {
			max = migration.Version
		}
	}
	if max == 0 {
		return 0, errors.New("db: no bundled migrations found")
	}
	return max, nil
}
