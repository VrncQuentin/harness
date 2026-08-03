package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

const traceRetentionDays = 30

// RetrievalTrace is one D3 trace row: one record per episode candidate per
// query call. query_id is a hash of the query text so no raw queries land in
// trace files. Emission happens inside ScoreEpisodePaths so the assembler path
// is measured as well as explicit memory_query calls.
type RetrievalTrace struct {
	QueryID       string    `json:"query_id"`
	EpisodePath   string    `json:"episode_path"`
	SemanticScore float64   `json:"semantic_score"`
	RecencyScore  float64   `json:"recency_score"`
	BlendedScore  float64   `json:"blended_score"`
	Rank          int       `json:"rank"`
	Ts            time.Time `json:"ts"`
}

// TraceSink receives D3 trace rows.
type TraceSink interface {
	Emit(RetrievalTrace)
	Close() error
}

// NopTraceSink discards all rows. Safe as the nil-value substitute.
type NopTraceSink struct{}

func (NopTraceSink) Emit(RetrievalTrace) {}
func (NopTraceSink) Close() error        { return nil }

// DefaultTraceSink is the package-level sink. ScoreEpisodePaths calls it on
// every candidate when non-nil. cmd/harness/main.go installs an NDJSONSink at
// startup; tests may set it temporarily.
var DefaultTraceSink TraceSink

// SetDefaultTraceSink replaces the package-level trace sink.
func SetDefaultTraceSink(s TraceSink) { DefaultTraceSink = s }

// QueryID returns an 8-hex-char prefix of SHA-256(query) so no raw query text
// lands in trace rows.
func QueryID(query string) string {
	h := sha256.Sum256([]byte(query))
	return hex.EncodeToString(h[:])[:8]
}

// NDJSONSink writes one JSON object per line to date-bucketed NDJSON files
// under dir. Files older than retentionDays are pruned on each date rotation.
// The now func is injectable for tests; nil uses time.Now.
//
// The trace directory is pinned for the sink's owned lifetime: construction
// opens it through rootfs, and every append, enumeration, and retention
// deletion happens through that pinned handle rather than by pathname. The
// pinned handle is closed by Close.
type NDJSONSink struct {
	root          *rootfs.Root
	retentionDays int
	now           func() time.Time

	mu  sync.Mutex
	day string
	f   *rootfs.AppendFile
}

// NewNDJSONSink returns a sink that writes under dir. It pins the directory,
// creating it and its missing ancestors through rooted operations if absent.
func NewNDJSONSink(dir string, now func() time.Time) (*NDJSONSink, error) {
	root, err := rootfs.OpenOrCreate(dir, 0o755)
	if err != nil {
		return nil, fmt.Errorf("retrieval: trace dir: %w", err)
	}
	s := &NDJSONSink{root: root, retentionDays: traceRetentionDays, now: now}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Emit writes t as a JSON line to the current day's file.
func (s *NDJSONSink) Emit(t RetrievalTrace) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := t.Ts.UTC().Format("2006-01-02")
	if err := s.ensureFile(day); err != nil {
		return
	}
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	_ = s.f.Write(b)
	_ = s.f.Write([]byte{'\n'})
}

// Close flushes the open file and releases the pinned trace directory.
func (s *NDJSONSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if s.root != nil {
		err := s.root.Close()
		s.root = nil
		return err
	}
	return nil
}

// ensureFile opens (or rotates to) the file for day through the pinned root.
// Caller holds mu.
func (s *NDJSONSink) ensureFile(day string) error {
	if s.f != nil && s.day == day {
		return nil
	}
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
		s.prune()
	}
	f, err := s.root.OpenAppend(day+".ndjson", 0o644)
	if err != nil {
		return fmt.Errorf("retrieval: open trace file: %w", err)
	}
	s.f = f
	s.day = day
	return nil
}

// prune removes files older than retentionDays. Caller holds mu.
func (s *NDJSONSink) prune() {
	s.pruneWithHook(nil)
}

// pruneWithHook is prune with a hook that runs after a candidate entry is
// observed and before it is removed, so a test can stage the substitution the
// identity verification exists to survive. The hook is a parameter rather than
// package state so parallel tests never see each other's. It is nil on every
// production path.
func (s *NDJSONSink) pruneWithHook(beforeRemove func(name string)) {
	cutoff := s.now().UTC().AddDate(0, 0, -s.retentionDays)
	entries, err := s.root.ReadDir(".")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		day := strings.TrimSuffix(e.Name(), ".ndjson")
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		if !t.Before(cutoff) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if beforeRemove != nil {
			beforeRemove(e.Name())
		}
		// Delete through the pinned root, and only the entry that was actually
		// observed: a stranger that has claimed the name since the listing is
		// detected by the identity comparison and refused rather than removed.
		_ = s.root.RemoveVerified(e.Name(), info)
	}
}
