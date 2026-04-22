package metrics

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := Open(db)
	if err != nil {
		t.Fatalf("metrics.Open: %v", err)
	}
	return store
}

func TestOpen_NilDB(t *testing.T) {
	if _, err := Open(nil); err == nil {
		t.Fatal("expected error for nil db, got nil")
	}
}

func TestOpenAndRecord(t *testing.T) {
	store := newTestStore(t)
	if err := store.Record("queue_depth", 3.0, map[string]string{"host": "local"}); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestQuery(t *testing.T) {
	store := newTestStore(t)

	before := time.Now().Add(-time.Second)

	for i := 0; i < 3; i++ {
		if err := store.Record("uptime_seconds", float64(i*10), nil); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	after := time.Now().Add(time.Second)

	pts, err := store.Query("uptime_seconds", before, after)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 3 {
		t.Errorf("expected 3 points, got %d", len(pts))
	}
}

func TestQuery_Empty(t *testing.T) {
	store := newTestStore(t)

	pts, err := store.Query("nonexistent", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("expected empty result, got %v", pts)
	}
}

func TestRecord_WithTags(t *testing.T) {
	store := newTestStore(t)

	tags := map[string]string{"process": "llama-server", "status": "healthy"}
	if err := store.Record("process_health", 1.0, tags); err != nil {
		t.Fatalf("record with tags: %v", err)
	}

	pts, err := store.Query("process_health", time.Now().Add(-time.Second), time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("expected at least one data point")
	}
	if pts[0].Tags["process"] != "llama-server" {
		t.Errorf("unexpected tag: %v", pts[0].Tags)
	}
}
