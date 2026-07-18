package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vrnc/harness/internal/metrics"
)

// MetricsStore is the time-series store for harness metrics.
type MetricsStore struct {
	db *sql.DB
}

var _ metrics.Store = (*MetricsStore)(nil)

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

// query returns data points for name in the half-open interval [from, to).
func (s *MetricsStore) query(name string, from, to time.Time) ([]metrics.DataPoint, error) {
	rows, err := s.db.Query(
		`SELECT name, value, tags, ts FROM metrics
		 WHERE name = ? AND ts >= ? AND ts < ?
		 UNION ALL
		 SELECT name, avg_value AS value, tags, hour_ts AS ts FROM metrics_hourly
		 WHERE name = ? AND hour_ts >= ? AND hour_ts < ?
		 ORDER BY ts ASC`,
		name, from.Unix(), to.Unix(),
		name, from.Unix(), to.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("db: query metric %s: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	return scanMetricRows(rows, "metric")
}

// Latest returns the newest sample for every metric name + tag set.
func (s *MetricsStore) Latest() ([]metrics.DataPoint, error) {
	rows, err := s.db.Query(
		`WITH points AS (
			 SELECT name, value, tags, ts, id * 2 + 1 AS ord FROM metrics
			 UNION ALL
			 SELECT name, last_value AS value, tags, hour_ts AS ts, id * 2 AS ord FROM metrics_hourly
		 ), ranked AS (
			 SELECT name, value, tags, ts,
				 ROW_NUMBER() OVER (PARTITION BY name, COALESCE(tags, '') ORDER BY ts DESC, ord DESC) AS rn
			 FROM points
		 )
		 SELECT name, value, tags, ts FROM ranked
		 WHERE rn = 1
		 ORDER BY name ASC, tags ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("db: latest metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMetricRows(rows, "latest metric")
}

// ApplyRetention downsamples raw rows older than retentionDays into hourly
// aggregates, then deletes those raw rows so harness.db cannot grow forever.
func (s *MetricsStore) ApplyRetention(retentionDays int) error {
	return s.applyRetentionAt(retentionDays, time.Now())
}

func (s *MetricsStore) applyRetentionAt(retentionDays int, now time.Time) error {
	if retentionDays < 1 {
		return fmt.Errorf("db: metrics retention days must be >= 1, got %d", retentionDays)
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	// Only downsample full hours. The cutoff hour remains raw until its end,
	// so a later retention pass never overwrites an aggregate with a partial hour.
	completeCutoff := cutoff.Truncate(time.Hour).Unix()
	updatedAt := now.Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("db: begin metrics retention: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(
		`WITH old AS (
			 SELECT name, COALESCE(tags, '') AS tags, value, ts, (ts / 3600) * 3600 AS hour_ts, id
			 FROM metrics
			 WHERE ts < ?
		 ), agg AS (
			 SELECT name, tags, hour_ts, COUNT(*) AS count, MIN(value) AS min_value,
				 MAX(value) AS max_value, AVG(value) AS avg_value
			 FROM old
			 GROUP BY name, tags, hour_ts
		 ), last AS (
			 SELECT name, tags, hour_ts, value,
				 ROW_NUMBER() OVER (PARTITION BY name, tags, hour_ts ORDER BY ts DESC, id DESC) AS rn
			 FROM old
		 )
		 INSERT INTO metrics_hourly(name, tags, hour_ts, count, min_value, max_value, avg_value, last_value, updated_at)
		 SELECT agg.name, agg.tags, agg.hour_ts, agg.count, agg.min_value, agg.max_value,
			 agg.avg_value, last.value, ?
		 FROM agg
		 JOIN last ON last.name = agg.name AND last.tags = agg.tags AND last.hour_ts = agg.hour_ts AND last.rn = 1
		 ON CONFLICT(name, tags, hour_ts) DO UPDATE SET
			 count = excluded.count,
			 min_value = excluded.min_value,
			 max_value = excluded.max_value,
			 avg_value = excluded.avg_value,
			 last_value = excluded.last_value,
			 updated_at = excluded.updated_at`,
		completeCutoff, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("db: downsample metrics: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM metrics WHERE ts < ?`, completeCutoff); err != nil {
		return fmt.Errorf("db: prune metrics: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit metrics retention: %w", err)
	}
	return nil
}

func scanMetricRows(rows *sql.Rows, label string) ([]metrics.DataPoint, error) {
	var pts []metrics.DataPoint
	for rows.Next() {
		var (
			dp       metrics.DataPoint
			tagsJSON string
			ts       int64
		)
		if err := rows.Scan(&dp.Name, &dp.Value, &tagsJSON, &ts); err != nil {
			return nil, fmt.Errorf("db: scan %s: %w", label, err)
		}
		dp.Time = time.Unix(ts, 0)
		if tagsJSON != "" {
			if err := json.Unmarshal([]byte(tagsJSON), &dp.Tags); err != nil {
				return nil, fmt.Errorf("db: unmarshal %s tags: %w", label, err)
			}
		}
		pts = append(pts, dp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: %s rows: %w", label, err)
	}
	return pts, nil
}
