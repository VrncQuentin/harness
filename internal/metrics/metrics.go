// Package metrics provides a SQLite-backed time-series store for harness metrics.
package metrics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register the sqlite driver
)

// Store is the interface for recording and querying metrics.
type Store interface {
	Record(name string, value float64, tags map[string]string) error
	Query(name string, from, to time.Time) ([]DataPoint, error)
}

// DataPoint is a single metric observation.
type DataPoint struct {
	Name  string
	Value float64
	Tags  map[string]string
	Time  time.Time
}

// sqliteStore implements Store using SQLite via modernc.org/sqlite.
type sqliteStore struct {
	db *sql.DB
}

// Open runs the metrics migration against the shared harness database. The
// caller owns db and is responsible for closing it.
func Open(db *sql.DB) (Store, error) {
	if db == nil {
		return nil, fmt.Errorf("metrics: nil db handle")
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS metrics (
	id    INTEGER PRIMARY KEY AUTOINCREMENT,
	name  TEXT    NOT NULL,
	value REAL    NOT NULL,
	tags  TEXT,
	ts    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS metrics_name_ts ON metrics(name, ts);
`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("metrics: migrate: %w", err)
	}
	return nil
}

// Record inserts a metric data point.
func (s *sqliteStore) Record(name string, value float64, tags map[string]string) error {
	var tagsJSON string
	if len(tags) > 0 {
		b, err := json.Marshal(tags)
		if err != nil {
			return fmt.Errorf("metrics: marshal tags: %w", err)
		}
		tagsJSON = string(b)
	}

	_, err := s.db.Exec(
		`INSERT INTO metrics(name, value, tags, ts) VALUES(?, ?, ?, ?)`,
		name, value, tagsJSON, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("metrics: record %s: %w", name, err)
	}
	return nil
}

// Query returns data points for name in the half-open interval [from, to).
func (s *sqliteStore) Query(name string, from, to time.Time) ([]DataPoint, error) {
	rows, err := s.db.Query(
		`SELECT name, value, tags, ts FROM metrics
		 WHERE name = ? AND ts >= ? AND ts < ?
		 ORDER BY ts ASC`,
		name, from.Unix(), to.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: query %s: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var pts []DataPoint
	for rows.Next() {
		var (
			dp       DataPoint
			tagsJSON string
			ts       int64
		)
		if err := rows.Scan(&dp.Name, &dp.Value, &tagsJSON, &ts); err != nil {
			return nil, fmt.Errorf("metrics: scan: %w", err)
		}
		dp.Time = time.Unix(ts, 0)
		if tagsJSON != "" {
			if err := json.Unmarshal([]byte(tagsJSON), &dp.Tags); err != nil {
				return nil, fmt.Errorf("metrics: unmarshal tags: %w", err)
			}
		}
		pts = append(pts, dp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: rows: %w", err)
	}
	return pts, nil
}
