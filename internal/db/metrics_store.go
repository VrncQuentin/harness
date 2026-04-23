package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vrnc/harness/internal/metrics"
)

// MetricsStore is the time-series store for harness metrics. It satisfies
// metrics.Store.
type MetricsStore struct {
	db *sql.DB
}

// Record inserts a metric data point.
func (s *MetricsStore) Record(name string, value float64, tags map[string]string) error {
	var tagsJSON string
	if len(tags) > 0 {
		b, err := json.Marshal(tags)
		if err != nil {
			return fmt.Errorf("db: marshal metric tags: %w", err)
		}
		tagsJSON = string(b)
	}

	_, err := s.db.Exec(
		`INSERT INTO metrics(name, value, tags, ts) VALUES(?, ?, ?, ?)`,
		name, value, tagsJSON, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: record metric %s: %w", name, err)
	}
	return nil
}

// Query returns data points for name in the half-open interval [from, to).
func (s *MetricsStore) Query(name string, from, to time.Time) ([]metrics.DataPoint, error) {
	rows, err := s.db.Query(
		`SELECT name, value, tags, ts FROM metrics
		 WHERE name = ? AND ts >= ? AND ts < ?
		 ORDER BY ts ASC`,
		name, from.Unix(), to.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("db: query metric %s: %w", name, err)
	}
	defer func() { _ = rows.Close() }()

	var pts []metrics.DataPoint
	for rows.Next() {
		var (
			dp       metrics.DataPoint
			tagsJSON string
			ts       int64
		)
		if err := rows.Scan(&dp.Name, &dp.Value, &tagsJSON, &ts); err != nil {
			return nil, fmt.Errorf("db: scan metric: %w", err)
		}
		dp.Time = time.Unix(ts, 0)
		if tagsJSON != "" {
			if err := json.Unmarshal([]byte(tagsJSON), &dp.Tags); err != nil {
				return nil, fmt.Errorf("db: unmarshal metric tags: %w", err)
			}
		}
		pts = append(pts, dp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: rows: %w", err)
	}
	return pts, nil
}
