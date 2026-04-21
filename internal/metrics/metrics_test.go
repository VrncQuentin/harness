package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAndRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if err := store.Record("queue_depth", 3.0, map[string]string{"host": "local"}); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestQuery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

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
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	pts, err := store.Query("nonexistent", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if pts != nil && len(pts) != 0 {
		t.Errorf("expected empty result, got %v", pts)
	}
}

func TestRecord_WithTags(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

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
