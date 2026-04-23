package metrics

import "time"

// Metric names used by Recorder. Kept as constants so callers that query
// metrics (dashboards, tests) reference the same strings.
const (
	MetricUptimeSeconds = "uptime_seconds"
	MetricQueueDepth    = "queue_depth"
	MetricProcessHealth = "process_health"
	MetricRestartCount  = "restart_count"
)

// TagProcess is the tag key identifying which managed process a sample
// belongs to (e.g. "llama-server", "embedder").
const TagProcess = "process"

// Recorder wraps a Store with typed methods for the metrics harness records.
// It centralises metric names and tag conventions so callers don't need to
// know the underlying string keys.
type Recorder struct {
	store Store
}

// NewRecorder returns a Recorder backed by the given Store.
func NewRecorder(store Store) *Recorder {
	return &Recorder{store: store}
}

// Uptime records how long the harness has been running, in seconds.
func (r *Recorder) Uptime(d time.Duration) error {
	return r.store.Record(MetricUptimeSeconds, d.Seconds(), nil)
}

// QueueDepth records the current request queue depth.
func (r *Recorder) QueueDepth(n int) error {
	return r.store.Record(MetricQueueDepth, float64(n), nil)
}

// ProcessHealth records whether a managed process is healthy (1.0) or not
// (0.0). The process name is attached as a tag.
func (r *Recorder) ProcessHealth(process string, healthy bool) error {
	v := 0.0
	if healthy {
		v = 1.0
	}
	return r.store.Record(MetricProcessHealth, v, map[string]string{TagProcess: process})
}

// ProcessRestartCount records the cumulative number of times a managed
// process has been restarted.
func (r *Recorder) ProcessRestartCount(process string, count int) error {
	return r.store.Record(MetricRestartCount, float64(count), map[string]string{TagProcess: process})
}
