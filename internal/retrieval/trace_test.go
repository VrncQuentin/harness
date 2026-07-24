package retrieval

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueryID_Deterministic(t *testing.T) {
	got1 := QueryID("hello world")
	got2 := QueryID("hello world")
	if got1 != got2 {
		t.Fatalf("QueryID not deterministic: %q vs %q", got1, got2)
	}
	if len(got1) != 16 {
		t.Fatalf("QueryID length = %d, want 16 hex chars", len(got1))
	}
}

func TestQueryID_Distinct(t *testing.T) {
	if QueryID("foo") == QueryID("bar") {
		t.Fatal("distinct queries must produce distinct QueryIDs")
	}
}

func TestNopTraceSink_Emit(t *testing.T) {
	var s NopTraceSink
	// Must not panic; nothing to assert about state.
	s.Emit(RetrievalTrace{QueryID: "abc", Candidate: "ep.md", Score: 0.5})
}

func TestNDJSONSink_WritesRow(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	s := &NDJSONSink{logDir: dir, retainDays: 7, now: func() time.Time { return fixed }}

	want := RetrievalTrace{
		QueryID: "aabbccdd11223344", Candidate: "episodes/a/x.md",
		Semantic: 0.8, Recency: 0.6, SWeight: 0.7, RWeight: 0.3, Score: 0.74, Returned: true,
	}
	s.Emit(want)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p := filepath.Join(dir, "retrieval", "2024-01-15.ndjson")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("open trace file: %v", err)
	}
	var got RetrievalTrace
	if err := json.Unmarshal(data[:len(data)-1], &got); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNDJSONSink_MultipleRowsSameDay(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	s := &NDJSONSink{logDir: dir, retainDays: 7, now: func() time.Time { return fixed }}

	for i := 0; i < 5; i++ {
		s.Emit(RetrievalTrace{QueryID: QueryID(string(rune('a' + i)))})
	}
	_ = s.Close()

	f, err := os.Open(filepath.Join(dir, "retrieval", "2024-06-01.ndjson"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var count int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() != "" {
			count++
		}
	}
	if count != 5 {
		t.Errorf("got %d rows, want 5", count)
	}
}

func TestNDJSONSink_Rotation(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)

	current := day1
	s := &NDJSONSink{logDir: dir, retainDays: 7, now: func() time.Time { return current }}

	s.Emit(RetrievalTrace{QueryID: "day1row"})
	current = day2
	s.Emit(RetrievalTrace{QueryID: "day2row"})
	_ = s.Close()

	for _, tc := range []struct {
		date string
		want string
	}{
		{"2024-01-15", "day1row"},
		{"2024-01-16", "day2row"},
	} {
		data, err := os.ReadFile(filepath.Join(dir, "retrieval", tc.date+".ndjson"))
		if err != nil {
			t.Fatalf("read %s: %v", tc.date, err)
		}
		line := data[:len(data)-1]
		var row RetrievalTrace
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("decode %s: %v", tc.date, err)
		}
		if row.QueryID != tc.want {
			t.Errorf("%s: got QueryID %q, want %q", tc.date, row.QueryID, tc.want)
		}
	}
}

func TestNDJSONSink_PrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	retrDir := filepath.Join(dir, "retrieval")
	if err := os.MkdirAll(retrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a file that will be 11 days old relative to day2.
	oldFile := filepath.Join(retrDir, "2024-01-01.ndjson")
	if err := os.WriteFile(oldFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	day1 := time.Date(2024, 1, 11, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	current := day1
	s := &NDJSONSink{logDir: dir, retainDays: 7, now: func() time.Time { return current }}
	s.Emit(RetrievalTrace{QueryID: "x"})
	current = day2
	s.Emit(RetrievalTrace{QueryID: "y"}) // triggers rotation → prune
	_ = s.Close()

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old file %s should have been pruned", oldFile)
	}
}

func TestNDJSONSink_CloseTwiceNoPanic(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &NDJSONSink{logDir: dir, retainDays: 7, now: func() time.Time { return fixed }}
	s.Emit(RetrievalTrace{QueryID: "a"})
	_ = s.Close()
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}
