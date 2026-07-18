// Package metrics defines the time-series types and Store interface for
// harness metrics. Persistence lives in internal/db - this package holds
// no SQL.
package metrics

import "time"

// Store is the interface for recording and querying metrics. The concrete
// SQLite implementation lives in internal/db.
type Store interface {
	Record(name string, value float64, tags map[string]string) error
	Latest() ([]DataPoint, error)
	ApplyRetention(retentionDays int) error
}

// DataPoint is a single metric observation.
type DataPoint struct {
	Name  string
	Value float64
	Tags  map[string]string
	Time  time.Time
}
