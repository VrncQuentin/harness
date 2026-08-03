package retrieval

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. Junctions need no privilege
// and are traversed exactly like symlinks, so they exercise the same escape on
// machines where symlink creation is denied.
func mustLinkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

func TestQueryIDDeterminism(t *testing.T) {
	a := QueryID("what is the capital of France")
	b := QueryID("what is the capital of France")
	if a != b {
		t.Fatalf("QueryID not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("QueryID length: want full SHA-256 (64 hex chars), got %d", len(a))
	}
}

func TestQueryIDDistinct(t *testing.T) {
	if QueryID("hello") == QueryID("world") {
		t.Fatal("QueryID collision for different inputs")
	}
}

func TestNopTraceSinkEmitAndClose(t *testing.T) {
	var s NopTraceSink
	s.Emit(RetrievalTrace{QueryID: "abc"})
	if err := s.Close(); err != nil {
		t.Fatalf("NopTraceSink.Close: %v", err)
	}
}

func TestNDJSONSinkWritesRow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}

	sink.Emit(RetrievalTrace{
		Version:     TraceSchemaVersion,
		RecordType:  RecordTypeCandidate,
		ProjectSlug: "global",
		QueryID:     "ab12ef34",
		Candidate:   "episodes/agent/2025-01-01.md",
		Score:       0.75,
		Timestamp:   now,
	})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p := filepath.Join(dir, "2025-01-15.ndjson")
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer func() { _ = f.Close() }()

	var row RetrievalTrace
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&row); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	if row.QueryID != "ab12ef34" {
		t.Errorf("QueryID: want ab12ef34, got %s", row.QueryID)
	}
	if row.Score != 0.75 {
		t.Errorf("Score: want 0.75, got %f", row.Score)
	}
	if row.Version != TraceSchemaVersion {
		t.Errorf("Version: want %d, got %d", TraceSchemaVersion, row.Version)
	}
	if row.RecordType != RecordTypeCandidate {
		t.Errorf("RecordType: want %s, got %s", RecordTypeCandidate, row.RecordType)
	}
	if row.ProjectSlug != "global" {
		t.Errorf("ProjectSlug: want global, got %s", row.ProjectSlug)
	}
}

func TestNDJSONSinkMultipleRowsSameDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}

	for i := range 3 {
		sink.Emit(RetrievalTrace{QueryID: "q", Rank: i, Timestamp: now})
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(filepath.Join(dir, "2025-03-01.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	count := 0
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("want 3 rows, got %d", count)
	}
}

func TestNDJSONSinkDayRotation(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2025, 4, 1, 6, 0, 0, 0, time.UTC)
	day2 := time.Date(2025, 4, 2, 6, 0, 0, 0, time.UTC)

	sink, err := NewNDJSONSink(dir, func() time.Time { return day1 })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}
	sink.Emit(RetrievalTrace{QueryID: "d1", Timestamp: day1})
	sink.Emit(RetrievalTrace{QueryID: "d2", Timestamp: day2})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "2025-04-01.ndjson")); err != nil {
		t.Errorf("day1 file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2025-04-02.ndjson")); err != nil {
		t.Errorf("day2 file missing: %v", err)
	}
}

func TestNDJSONSinkPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()

	// Pre-create a 40-day-old file.
	old := "2020-01-01.ndjson"
	if err := os.WriteFile(filepath.Join(dir, old), []byte(`{"query_id":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2020, 2, 15, 0, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}
	// Emit on a new day so rotation triggers pruning.
	newDay := time.Date(2020, 2, 16, 0, 0, 0, 0, time.UTC)
	sink.Emit(RetrievalTrace{QueryID: "new", Timestamp: now})
	sink.Emit(RetrievalTrace{QueryID: "new2", Timestamp: newDay})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, old)); err == nil {
		t.Error("old file should have been pruned")
	}
}

func TestNDJSONSinkCloseTwice(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(dir, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}
	sink.Emit(RetrievalTrace{QueryID: "x", Timestamp: now})
	if err := sink.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// The sink must pin its trace directory for its owned lifetime.
// After construction, re-pointing the configured name at another directory
// must not redirect appends: the row lands in the directory the handle was
// pinned on, never the replacement.
func TestNDJSONSink_TraceDirectoryIsPinned(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(base, "trace")
	mustLinkDir(t, real, trace)
	defer func() { _ = os.Remove(trace) }()

	now := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(trace, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}
	defer func() { _ = sink.Close() }()

	// Re-point the configured directory after the sink pinned it.
	if err := os.Remove(trace); err != nil {
		t.Fatal(err)
	}
	mustLinkDir(t, evil, trace)

	sink.Emit(RetrievalTrace{QueryID: "abc", Timestamp: now})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(real, "2025-07-01.ndjson")); err != nil {
		t.Errorf("trace row missing from the pinned directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(evil, "2025-07-01.ndjson")); err == nil {
		t.Error("trace row written into the repointed directory")
	}
}

// Retention must not delete a stranger that claims a previously
// observed name. The sink observes an old trace entry during its listing; a
// stranger then takes the name over (here via a hard link to a file it cares
// about). Deletion is refused because the name no longer identifies the entry
// the listing observed, so the stranger's data survives on every name.
func TestNDJSONSink_RetentionDeletesOnlyOwnFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2020, 2, 15, 0, 0, 0, 0, time.UTC)
	sink, err := NewNDJSONSink(dir, func() time.Time { return newer })
	if err != nil {
		t.Fatalf("NewNDJSONSink: %v", err)
	}
	defer func() { _ = sink.Close() }()

	// The sink creates the old-day file itself, so the observed name is its own.
	sink.Emit(RetrievalTrace{QueryID: "old", Timestamp: old})

	// A stranger's file outside the tree, and a hard link that claims the old
	// trace entry between enumeration and removal.
	stranger := filepath.Join(dir, "stranger.bin")
	if err := os.WriteFile(stranger, []byte("stranger-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTrace := filepath.Join(dir, "2020-01-01.ndjson")

	substituted := false
	sink.pruneWithHook(func(name string) {
		if name != "2020-01-01.ndjson" || substituted {
			return
		}
		substituted = true
		if err := os.Remove(oldTrace); err != nil {
			t.Fatalf("remove observed entry: %v", err)
		}
		if err := os.Link(stranger, oldTrace); err != nil {
			t.Fatalf("link stranger over observed name: %v", err)
		}
	})

	if !substituted {
		t.Fatal("the hook never ran; the substitution was not staged")
	}
	// The stranger's original name must survive (its inode was not deleted).
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("stranger file was removed: %v", err)
	}
	// The claimed name must survive too: it now identifies the stranger's file,
	// and the observed entry was a different one.
	if _, err := os.Stat(oldTrace); err != nil {
		t.Errorf("the claimed name was removed despite no longer identifying the observed entry: %v", err)
	}

	// Control: a prune with no substitution still deletes the sink's own old
	// entry, so the identity check is what protects strangers, not a refusal to
	// prune altogether.
	control := t.TempDir()
	csink, err := NewNDJSONSink(control, func() time.Time { return newer })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = csink.Close() }()
	if err := os.WriteFile(filepath.Join(control, "2020-01-01.ndjson"), []byte("own\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	csink.pruneWithHook(nil)
	if _, err := os.Stat(filepath.Join(control, "2020-01-01.ndjson")); err == nil {
		t.Error("the sink's own expired entry should have been pruned")
	}
}
