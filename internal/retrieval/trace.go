package retrieval

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

const traceRetentionDays = 30

// TraceSchemaVersion is the schema version carried by every trace row. Trace
// consumers must reject rows whose version they do not recognize rather than
// guess at the fields.
const TraceSchemaVersion = 1

// Record types for RetrievalTrace.RecordType. Every retrieval invocation emits
// exactly one "call" record; a scoreable invocation additionally emits one
// "candidate" record per scored episode.
const (
	RecordTypeCall      = "call"
	RecordTypeCandidate = "candidate"
)

// Call-row outcomes for RetrievalTrace.Outcome. unscoreable covers blank
// queries, unavailable dependencies, and empty result sets; error covers
// embed/search failures; scored means at least one candidate received a score.
const (
	OutcomeScored      = "scored"
	OutcomeUnscoreable = "unscoreable"
	OutcomeError       = "error"
)

// TraceContext carries the identity and requested top-K of one retrieval
// invocation. Callers pass it to ScoreEpisodePaths so every emitted row is
// namespaced by project and Returned has one unambiguous meaning. A zero-value
// TraceContext (empty ProjectSlug) disables emission.
type TraceContext struct {
	// ProjectSlug namespaces project-relative episode paths so identical
	// relative paths in different projects never collide.
	ProjectSlug string
	// TopK is the caller's requested top-K. A candidate is Returned when its
	// final rank is within TopK; TopK <= 0 means unlimited (every scored
	// candidate is returned).
	TopK int
}

// RetrievalTrace is one versioned D3 trace row. Every retrieval invocation
// emits one "call" record carrying the outcome; a scoreable invocation with
// candidates additionally emits one "candidate" record per episode. query_id is
// a full SHA-256 hex of the query text so no raw queries land in trace files;
// invocation_id is a fresh opaque identifier minted per call, shared by the
// call record and every candidate record so the two are associated even when
// identical queries repeat or concurrent emissions interleave. Emission happens
// inside ScoreEpisodePaths so the assembler path is measured as well as explicit
// memory_query calls.
type RetrievalTrace struct {
	Version        int       `json:"version"`
	RecordType     string    `json:"record_type"`
	InvocationID   string    `json:"invocation_id"`
	ProjectSlug    string    `json:"project_slug"`
	QueryID        string    `json:"query_id"`
	Candidate      string    `json:"candidate"`
	Semantic       float64   `json:"semantic"`
	Recency        float64   `json:"recency"`
	SemanticWeight float64   `json:"semantic_weight"`
	RecencyWeight  float64   `json:"recency_weight"`
	Score          float64   `json:"score"`
	Rank           int       `json:"rank"`
	Returned       bool      `json:"returned"`
	Outcome        string    `json:"outcome"`
	Timestamp      time.Time `json:"timestamp"`
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

// QueryID returns the full SHA-256 hex of query so no raw query text lands in
// trace rows and equal queries in different projects are still identifiable
// without the raw text.
func QueryID(query string) string {
	h := sha256.Sum256([]byte(query))
	return hex.EncodeToString(h[:])
}

// NewInvocationID mints a fresh opaque identifier for one retrieval call. It is
// shared by the call record and every candidate record so a trace consumer can
// associate candidates with their invocation even when the same query repeats
// within a project or concurrent emissions interleave.
func NewInvocationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("inv-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
	day := t.Timestamp.UTC().Format("2006-01-02")
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

// Close flushes the open file and releases the pinned trace directory. A
// failure closing the append file is preserved alongside any failure closing
// the root, so a shutdown cannot report success after a file close that failed.
func (s *NDJSONSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if s.f != nil {
		if err := s.f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("retrieval: close trace file: %w", err))
		}
		s.f = nil
	}
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			errs = append(errs, err)
		}
		s.root = nil
	}
	return errors.Join(errs...)
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
