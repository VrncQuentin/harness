package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vrnc/harness/internal/config"
)

// newTestDB opens a fresh harness.db in a temp dir and registers cleanup.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpen_CreatesTablesAndSeedsConfigRow(t *testing.T) {
	d := newTestDB(t)

	var count int
	if err := d.sqldb.QueryRow("SELECT COUNT(*) FROM config").Scan(&count); err != nil {
		t.Fatalf("count config rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 config row after Open, got %d", count)
	}

	// metrics table should exist even if empty.
	if err := d.sqldb.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&count); err != nil {
		t.Fatalf("count metrics rows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 metrics rows after Open, got %d", count)
	}
}

func TestOpen_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = d2.Close() }()

	var count int
	if err := d2.sqldb.QueryRow("SELECT COUNT(*) FROM config").Scan(&count); err != nil {
		t.Fatalf("count config rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 config row after reopen, got %d", count)
	}
}

func TestPeekUIPort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.db")

	if got := PeekUIPort(path, 3000); got != 3000 {
		t.Fatalf("missing DB port = %d, want fallback 3000", got)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cfg, _, err := d.Config().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Model.Binary = "C:\\llama.exe"
	cfg.Model.ModelPath = "C:\\model.gguf"
	cfg.Embedder.Binary = "C:\\embed.exe"
	cfg.Embedder.ModelPath = "C:\\embed.gguf"
	cfg.UI.Port = 31337
	if err := d.Config().Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := PeekUIPort(path, 3000); got != 31337 {
		t.Fatalf("saved DB port = %d, want 31337", got)
	}
}

func TestConfigStore_LoadFreshReturnsDefaultsAndNotConfigured(t *testing.T) {
	d := newTestDB(t)

	cfg, configured, err := d.Config().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if configured {
		t.Error("expected configured=false on fresh store")
	}

	defaults := config.Defaults()
	if cfg.Model.CtxSize != defaults.Model.CtxSize {
		t.Errorf("Model.CtxSize: got %d, want %d", cfg.Model.CtxSize, defaults.Model.CtxSize)
	}
	if cfg.Model.Port != defaults.Model.Port {
		t.Errorf("Model.Port: got %d, want %d", cfg.Model.Port, defaults.Model.Port)
	}
	if cfg.UI.Port != defaults.UI.Port {
		t.Errorf("UI.Port: got %d, want %d", cfg.UI.Port, defaults.UI.Port)
	}
	if cfg.UI.OpenOnStart != defaults.UI.OpenOnStart {
		t.Errorf("UI.OpenOnStart: got %v, want %v", cfg.UI.OpenOnStart, defaults.UI.OpenOnStart)
	}
	if cfg.Queue.MaxDepth != defaults.Queue.MaxDepth {
		t.Errorf("Queue.MaxDepth: got %d, want %d", cfg.Queue.MaxDepth, defaults.Queue.MaxDepth)
	}
	if cfg.Metrics.RetentionDays != defaults.Metrics.RetentionDays {
		t.Errorf("Metrics.RetentionDays: got %d, want %d", cfg.Metrics.RetentionDays, defaults.Metrics.RetentionDays)
	}
	if cfg.Log.RingMaxEntries != defaults.Log.RingMaxEntries {
		t.Errorf("Log.RingMaxEntries: got %d, want %d", cfg.Log.RingMaxEntries, defaults.Log.RingMaxEntries)
	}
	if cfg.Log.ProcMaxLines != defaults.Log.ProcMaxLines {
		t.Errorf("Log.ProcMaxLines: got %d, want %d", cfg.Log.ProcMaxLines, defaults.Log.ProcMaxLines)
	}
	if cfg.Agent.Active != "" {
		t.Errorf("Agent.Active default: got %q, want empty", cfg.Agent.Active)
	}
}

func TestConfigStore_SaveMarksConfiguredAndRoundTrips(t *testing.T) {
	d := newTestDB(t)
	store := d.Config()

	cfg := config.Defaults()
	cfg.Model.Binary = "C:\\llama.exe"
	cfg.Model.ModelPath = "C:\\m.gguf"
	cfg.Model.CtxSize = 4096
	cfg.Model.Verbose = true
	cfg.Embedder.Binary = "C:\\embed.exe"
	cfg.Embedder.ModelPath = "C:\\e.gguf"
	cfg.Embedder.Verbose = true
	cfg.Memory.RepoPath = "C:\\memory"
	cfg.Agent.Active = "coder"
	cfg.UI.OpenOnStart = false
	cfg.API.Enabled = true
	cfg.API.Port = 9090
	cfg.Log.RingMaxEntries = 1234
	cfg.Log.ProcMaxLines = 99

	if err := store.Save(&cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, configured, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !configured {
		t.Error("expected configured=true after Save")
	}
	if loaded.Model.Binary != cfg.Model.Binary {
		t.Errorf("Model.Binary roundtrip: got %q, want %q", loaded.Model.Binary, cfg.Model.Binary)
	}
	if loaded.Model.CtxSize != 4096 {
		t.Errorf("Model.CtxSize roundtrip: got %d, want 4096", loaded.Model.CtxSize)
	}
	if loaded.UI.OpenOnStart {
		t.Errorf("UI.OpenOnStart roundtrip: got true, want false")
	}
	if !loaded.API.Enabled {
		t.Errorf("API.Enabled roundtrip: got false, want true")
	}
	if loaded.API.Port != 9090 {
		t.Errorf("API.Port roundtrip: got %d, want 9090", loaded.API.Port)
	}
	if loaded.Memory.RepoPath != cfg.Memory.RepoPath {
		t.Errorf("Memory.RepoPath roundtrip: got %q, want %q", loaded.Memory.RepoPath, cfg.Memory.RepoPath)
	}
	if loaded.Agent.Active != "coder" {
		t.Errorf("Agent.Active roundtrip: got %q, want %q", loaded.Agent.Active, "coder")
	}
	if loaded.Log.RingMaxEntries != 1234 {
		t.Errorf("Log.RingMaxEntries roundtrip: got %d, want 1234", loaded.Log.RingMaxEntries)
	}
	if loaded.Log.ProcMaxLines != 99 {
		t.Errorf("Log.ProcMaxLines roundtrip: got %d, want 99", loaded.Log.ProcMaxLines)
	}
	if !loaded.Model.Verbose {
		t.Errorf("Model.Verbose roundtrip: got false, want true")
	}
	if !loaded.Embedder.Verbose {
		t.Errorf("Embedder.Verbose roundtrip: got false, want true")
	}
}

func TestConfigStore_SaveNilRejected(t *testing.T) {
	d := newTestDB(t)
	if err := d.Config().Save(nil); err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestMetricsStore_RecordAndQuery(t *testing.T) {
	d := newTestDB(t)
	store := d.Metrics()

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

func TestMetricsStore_QueryEmpty(t *testing.T) {
	d := newTestDB(t)
	pts, err := d.Metrics().Query("nonexistent", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("expected empty result, got %v", pts)
	}
}

func TestMetricsStore_RecordWithTags(t *testing.T) {
	d := newTestDB(t)
	store := d.Metrics()

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
