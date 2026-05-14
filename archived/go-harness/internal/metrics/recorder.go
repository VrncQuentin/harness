package metrics

import "time"

// Metric names used by Recorder. Kept as constants so callers that query
// metrics (dashboards, tests) reference the same strings.
const (
	MetricUptimeSeconds      = "uptime_seconds"
	MetricQueueDepth         = "queue_depth"
	MetricProcessHealth      = "process_health"
	MetricRestartCount       = "restart_count"
	MetricSessionCount       = "session_count"
	MetricEpisodeCount       = "episode_count"
	MetricGitCommitLatencyMS = "git_commit_latency_ms"
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

// SessionCount records the number of distinct session ids the harness
// has saved at least once. Reported as a gauge: callers pass the
// running total, not a delta.
func (r *Recorder) SessionCount(n int) error {
	return r.store.Record(MetricSessionCount, float64(n), nil)
}

// EpisodeCount records the number of distinct episode files committed
// to the memory repo. Reported as a gauge: callers pass the running
// total, not a delta.
func (r *Recorder) EpisodeCount(n int) error {
	return r.store.Record(MetricEpisodeCount, float64(n), nil)
}

// GitCommitLatencyMS records the wall-clock duration of a single git
// commit in milliseconds. Each Save observes one sample, written with
// no tags so the UI can chart the raw histogram.
func (r *Recorder) GitCommitLatencyMS(d time.Duration) error {
	ms := float64(d) / float64(time.Millisecond)
	return r.store.Record(MetricGitCommitLatencyMS, ms, nil)
}
