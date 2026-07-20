package session

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadAll_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadAll(filepath.Join(dir, "sessions.jsonl"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result for missing file, got %d records", len(got))
	}
}

func TestAppendRecordAndReadAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")

	rec := Record{
		ID:          "2026-04-26T22-15-03Z",
		Agent:       "coder",
		Project:     "global",
		StartedAt:   time.Date(2026, 4, 26, 22, 14, 0, 0, time.UTC),
		SavedAt:     time.Date(2026, 4, 26, 22, 15, 3, 0, time.UTC),
		SaveSeq:     1,
		EpisodePath: "episodes/coder/2026-04-26T22-15-03Z.md",
	}
	if err := AppendRecord(path, rec); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	rec.SaveSeq = 2
	rec.SavedAt = rec.SavedAt.Add(time.Minute)
	if err := AppendRecord(path, rec); err != nil {
		t.Fatalf("AppendRecord 2: %v", err)
	}

	got, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[1].SaveSeq != 2 {
		t.Errorf("second record save_seq: want 2, got %d", got[1].SaveSeq)
	}
}

func TestReadAll_SkipsGarbledLine(t *testing.T) {
	// Capture slog warnings so the test asserts the recovery path is
	// audible in production.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.jsonl")
	good := Record{
		ID:        "2026-04-26T22-15-03Z",
		Agent:     "coder",
		Project:   "global",
		StartedAt: time.Date(2026, 4, 26, 22, 14, 0, 0, time.UTC),
		SavedAt:   time.Date(2026, 4, 26, 22, 15, 3, 0, time.UTC),
		SaveSeq:   1,
	}
	if err := AppendRecord(path, good); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	// Corrupt the file: append garbage that is not valid JSON.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString("not-json-garbage\n"); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	_ = f.Close()

	got, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 parseable record, got %d", len(got))
	}
	if !bytes.Contains(buf.Bytes(), []byte("garbled sessions.jsonl")) {
		t.Errorf("expected slog warn about garbled line, got: %s", buf.String())
	}
}

func TestLatestPerID(t *testing.T) {
	now := time.Now().UTC()
	records := []Record{
		{ID: "a", SaveSeq: 1, SavedAt: now},
		{ID: "b", SaveSeq: 1, SavedAt: now.Add(time.Second)},
		{ID: "a", SaveSeq: 2, SavedAt: now.Add(2 * time.Second)},
		{ID: "a", SaveSeq: 3, SavedAt: now.Add(3 * time.Second)},
		{ID: "c", SaveSeq: 1, SavedAt: now.Add(4 * time.Second)},
	}
	got := LatestPerID(records)
	if len(got) != 3 {
		t.Fatalf("expected 3 deduped records, got %d", len(got))
	}
	wantIDs := []string{"a", "b", "c"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("dedupe[%d]: want %q, got %q", i, want, got[i].ID)
		}
	}
	if got[0].SaveSeq != 3 {
		t.Errorf("a's winning seq: want 3, got %d", got[0].SaveSeq)
	}
}

func TestSortByNewestOrdersBySavedAtThenID(t *testing.T) {
	now := time.Now().UTC()
	records := []Record{
		{ID: "c", SavedAt: now},
		{ID: "b", SavedAt: now.Add(time.Hour)},
		{ID: "a", SavedAt: now},
		{ID: "d", SavedAt: now.Add(-time.Hour)},
	}
	sortByNewest(records)
	want := []string{"b", "a", "c", "d"}
	for i, id := range want {
		if records[i].ID != id {
			t.Fatalf("sort[%d] = %q, want %q; full order = [%s,%s,%s,%s]", i, records[i].ID, id, records[0].ID, records[1].ID, records[2].ID, records[3].ID)
		}
	}
}
