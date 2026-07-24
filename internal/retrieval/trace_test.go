package retrieval

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueryIDDeterminism(t *testing.T) {
	a := QueryID("what is the capital of France")
	b := QueryID("what is the capital of France")
	if a != b {
		t.Fatalf("QueryID not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("QueryID length: want 8, got %d", len(a))
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
		QueryID:     "ab12ef34",
		EpisodePath: "episodes/agent/2025-01-01.md",
		BlendedScore: 0.75,
		Ts:           now,
	})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p := filepath.Join(dir, "2025-01-15.ndjson")
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	defer f.Close()

	var row RetrievalTrace
	if err := json.NewDecoder(bufio.NewReader(f)).Decode(&row); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	if row.QueryID != "ab12ef34" {
		t.Errorf("QueryID: want ab12ef34, got %s", row.QueryID)
	}
	if row.BlendedScore != 0.75 {
		t.Errorf("BlendedScore: want 0.75, got %f", row.BlendedScore)
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
		sink.Emit(RetrievalTrace{QueryID: "q", Rank: i, Ts: now})
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(filepath.Join(dir, "2025-03-01.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

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
	sink.Emit(RetrievalTrace{QueryID: "d1", Ts: day1})
	sink.Emit(RetrievalTrace{QueryID: "d2", Ts: day2})
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
	sink.Emit(RetrievalTrace{QueryID: "new", Ts: now})
	sink.Emit(RetrievalTrace{QueryID: "new2", Ts: newDay})
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
	sink.Emit(RetrievalTrace{QueryID: "x", Ts: now})
	if err := sink.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
