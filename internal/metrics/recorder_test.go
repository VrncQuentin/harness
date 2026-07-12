package metrics

import (
	"errors"
	"testing"
	"time"
)

// fakeStore captures Record calls for assertion. Query is unused here.
type fakeStore struct {
	calls []recordCall
	err   error
}

type recordCall struct {
	name  string
	value float64
	tags  map[string]string
}

func (f *fakeStore) Record(name string, value float64, tags map[string]string) error {
	f.calls = append(f.calls, recordCall{name: name, value: value, tags: tags})
	return f.err
}

func (f *fakeStore) Query(string, time.Time, time.Time) ([]DataPoint, error) {
	return nil, nil
}

func (f *fakeStore) Latest() ([]DataPoint, error) {
	return nil, nil
}

func TestRecorder_Uptime(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.Uptime(90 * time.Second); err != nil {
		t.Fatalf("Uptime: %v", err)
	}

	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricUptimeSeconds {
		t.Errorf("name: want %q, got %q", MetricUptimeSeconds, c.name)
	}
	if c.value != 90.0 {
		t.Errorf("value: want 90, got %v", c.value)
	}
	if c.tags != nil {
		t.Errorf("tags: want nil, got %v", c.tags)
	}
}

func TestRecorder_QueueDepth(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.QueueDepth(7); err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricQueueDepth {
		t.Errorf("name: want %q, got %q", MetricQueueDepth, c.name)
	}
	if c.value != 7.0 {
		t.Errorf("value: want 7, got %v", c.value)
	}
	if c.tags != nil {
		t.Errorf("tags: want nil, got %v", c.tags)
	}
}

func TestRecorder_ProcessHealth(t *testing.T) {
	tests := []struct {
		name    string
		process string
		healthy bool
		want    float64
	}{
		{"healthy", "llama-server", true, 1.0},
		{"unhealthy", "embedder", false, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fakeStore{}
			r := NewRecorder(fs)

			if err := r.ProcessHealth(tt.process, tt.healthy); err != nil {
				t.Fatalf("ProcessHealth: %v", err)
			}
			if len(fs.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(fs.calls))
			}
			c := fs.calls[0]
			if c.name != MetricProcessHealth {
				t.Errorf("name: want %q, got %q", MetricProcessHealth, c.name)
			}
			if c.value != tt.want {
				t.Errorf("value: want %v, got %v", tt.want, c.value)
			}
			if c.tags[TagProcess] != tt.process {
				t.Errorf("tag %q: want %q, got %q", TagProcess, tt.process, c.tags[TagProcess])
			}
		})
	}
}

func TestRecorder_ProcessRestartCount(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.ProcessRestartCount("llama-server", 3); err != nil {
		t.Fatalf("ProcessRestartCount: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricRestartCount {
		t.Errorf("name: want %q, got %q", MetricRestartCount, c.name)
	}
	if c.value != 3.0 {
		t.Errorf("value: want 3, got %v", c.value)
	}
	if c.tags[TagProcess] != "llama-server" {
		t.Errorf("tag %q: want %q, got %q", TagProcess, "llama-server", c.tags[TagProcess])
	}
}

func TestRecorder_PropagatesStoreError(t *testing.T) {
	want := errors.New("boom")
	fs := &fakeStore{err: want}
	r := NewRecorder(fs)

	if err := r.Uptime(time.Second); !errors.Is(err, want) {
		t.Errorf("Uptime: want %v, got %v", want, err)
	}
	if err := r.QueueDepth(1); !errors.Is(err, want) {
		t.Errorf("QueueDepth: want %v, got %v", want, err)
	}
	if err := r.ProcessHealth("p", true); !errors.Is(err, want) {
		t.Errorf("ProcessHealth: want %v, got %v", want, err)
	}
	if err := r.ProcessRestartCount("p", 1); !errors.Is(err, want) {
		t.Errorf("ProcessRestartCount: want %v, got %v", want, err)
	}
	if err := r.SessionCount(1); !errors.Is(err, want) {
		t.Errorf("SessionCount: want %v, got %v", want, err)
	}
	if err := r.EpisodeCount(1); !errors.Is(err, want) {
		t.Errorf("EpisodeCount: want %v, got %v", want, err)
	}
	if err := r.GitCommitLatencyMS(time.Millisecond); !errors.Is(err, want) {
		t.Errorf("GitCommitLatencyMS: want %v, got %v", want, err)
	}
}

func TestRecorder_SessionCount(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.SessionCount(4); err != nil {
		t.Fatalf("SessionCount: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricSessionCount {
		t.Errorf("name: want %q, got %q", MetricSessionCount, c.name)
	}
	if c.value != 4.0 {
		t.Errorf("value: want 4, got %v", c.value)
	}
	if c.tags != nil {
		t.Errorf("tags: want nil, got %v", c.tags)
	}
}

func TestRecorder_EpisodeCount(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.EpisodeCount(11); err != nil {
		t.Fatalf("EpisodeCount: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricEpisodeCount {
		t.Errorf("name: want %q, got %q", MetricEpisodeCount, c.name)
	}
	if c.value != 11.0 {
		t.Errorf("value: want 11, got %v", c.value)
	}
}

func TestRecorder_GitCommitLatencyMS(t *testing.T) {
	fs := &fakeStore{}
	r := NewRecorder(fs)

	if err := r.GitCommitLatencyMS(250 * time.Millisecond); err != nil {
		t.Fatalf("GitCommitLatencyMS: %v", err)
	}
	if len(fs.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fs.calls))
	}
	c := fs.calls[0]
	if c.name != MetricGitCommitLatencyMS {
		t.Errorf("name: want %q, got %q", MetricGitCommitLatencyMS, c.name)
	}
	if c.value != 250.0 {
		t.Errorf("value: want 250, got %v", c.value)
	}
}
