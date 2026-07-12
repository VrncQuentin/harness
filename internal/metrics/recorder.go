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
	MetricTTFTMS             = "ttft_ms"
	MetricTokenThroughput    = "token_throughput_tokens_per_sec"
	MetricVRAMUsedMB         = "vram_used_mb"
	MetricLoopTurnCount      = "loop_turn_count"
	MetricToolCallCount      = "tool_call_count"
	MetricToolCallErrorCount = "tool_call_error_count"
	MetricToolCallErrorRate  = "tool_call_error_rate"
)

// TagProcess is the tag key identifying which managed process a sample
// belongs to (e.g. "llama-server", "embedder").
const TagProcess = "process"

// TagTool is the tag key identifying which agent-loop tool emitted a sample.
const TagTool = "tool"

// TagGPU is the tag key identifying the GPU index for hardware metrics.
const TagGPU = "gpu"

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

// TimeToFirstTokenMS records model latency from dispatch to first streamed token.
func (r *Recorder) TimeToFirstTokenMS(d time.Duration) error {
	ms := float64(d) / float64(time.Millisecond)
	return r.store.Record(MetricTTFTMS, ms, nil)
}

// TokenThroughput records streamed text-token throughput for one request.
func (r *Recorder) TokenThroughput(tokensPerSecond float64) error {
	return r.store.Record(MetricTokenThroughput, tokensPerSecond, nil)
}

// VRAMUsedMB records used GPU memory in MiB as reported by nvidia-smi.
func (r *Recorder) VRAMUsedMB(gpu string, mb float64) error {
	return r.store.Record(MetricVRAMUsedMB, mb, map[string]string{TagGPU: gpu})
}

// LoopTurn records one completed agent-loop turn.
func (r *Recorder) LoopTurn() error {
	return r.store.Record(MetricLoopTurnCount, 1, nil)
}

// ToolCall records one tool call attempt.
func (r *Recorder) ToolCall(tool string) error {
	return r.store.Record(MetricToolCallCount, 1, map[string]string{TagTool: tool})
}

// ToolCallError records one failed tool call.
func (r *Recorder) ToolCallError(tool string) error {
	return r.store.Record(MetricToolCallErrorCount, 1, map[string]string{TagTool: tool})
}

// ToolCallErrorRate records whether a tool call failed: 1 for error, 0 for success.
func (r *Recorder) ToolCallErrorRate(tool string, failed bool) error {
	v := 0.0
	if failed {
		v = 1.0
	}
	return r.store.Record(MetricToolCallErrorRate, v, map[string]string{TagTool: tool})
}
