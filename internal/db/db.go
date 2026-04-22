// Package db owns every piece of SQL in the harness: the SQLite driver
// registration, connection opening, migration runner, and all CRUD
// statements used by other subsystems. No other package imports
// database/sql or a SQL driver.
package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite" // register the sqlite driver

	"github.com/vrnc/harness/migrations"
)

// DB owns the shared harness SQLite handle and exposes typed stores for each
// subsystem. Callers open it once in main and pass the sub-stores around.
type DB struct {
	sqldb   *sql.DB
	cfg     *ConfigStore
	metrics *MetricsStore
}

// Open opens harness.db at path, applies any pending migrations, and seeds
// the singleton config row. The returned *DB must be closed via Close.
func Open(path string) (*DB, error) {
	sqldb, err := sql.Open("sqlite", path)
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
	if err := seed(sqldb); err != nil {
		_ = sqldb.Close()
		return nil, err
	}

	d := &DB{sqldb: sqldb}
	d.cfg = &ConfigStore{db: sqldb}
	d.metrics = &MetricsStore{db: sqldb}
	return d, nil
}

// Close closes the underlying SQLite handle.
func (d *DB) Close() error {
	return d.sqldb.Close()
}

// Config returns the config sub-store.
func (d *DB) Config() *ConfigStore { return d.cfg }

// Metrics returns the metrics sub-store.
func (d *DB) Metrics() *MetricsStore { return d.metrics }

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

// seed inserts the singleton config row if it doesn't exist. Column defaults
// in the DDL supply the initial values; they must stay in sync with
// config.Defaults.
func seed(sqldb *sql.DB) error {
	if _, err := sqldb.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		return fmt.Errorf("db: seed config: %w", err)
	}
	return nil
}
