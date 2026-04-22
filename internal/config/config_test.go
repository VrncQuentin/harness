package config

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestStore returns an initialized Store backed by a temp SQLite file and a
// cleanup that closes it.
func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store, err := Open(db)
	if err != nil {
		t.Fatalf("config.Open: %v", err)
	}
	return store, db
}

func TestOpen_NilDB(t *testing.T) {
	if _, err := Open(nil); err == nil {
		t.Fatal("expected error for nil db, got nil")
	}
}

func TestOpen_CreatesTableAndSeedsRow(t *testing.T) {
	_, db := newTestStore(t)

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM config").Scan(&count); err != nil {
		t.Fatalf("count config rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 config row after Open, got %d", count)
	}
}

func TestOpen_Idempotent(t *testing.T) {
	store, db := newTestStore(t)

	// Calling Open again on the same DB must not duplicate the row or fail.
	if _, err := Open(db); err != nil {
		t.Fatalf("second Open: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM config").Scan(&count); err != nil {
		t.Fatalf("count config rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 config row after reopen, got %d", count)
	}
	_ = store
}

func TestLoad_FreshReturnsDefaultsAndNotConfigured(t *testing.T) {
	store, _ := newTestStore(t)

	cfg, configured, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if configured {
		t.Error("expected configured=false on fresh store")
	}

	defaults := Defaults()
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
}

func TestSave_MarksConfiguredAndRoundTrips(t *testing.T) {
	store, _ := newTestStore(t)

	cfg := Defaults()
	cfg.Model.Binary = "C:\\llama.exe"
	cfg.Model.ModelPath = "C:\\m.gguf"
	cfg.Model.CtxSize = 4096
	cfg.Embedder.Binary = "C:\\embed.exe"
	cfg.Embedder.ModelPath = "C:\\e.gguf"
	cfg.Memory.RepoPath = "C:\\memory"
	cfg.UI.OpenOnStart = false
	cfg.API.Enabled = true
	cfg.API.Port = 9090

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
	if loaded.UI.OpenOnStart != false {
		t.Errorf("UI.OpenOnStart roundtrip: got %v, want false", loaded.UI.OpenOnStart)
	}
	if loaded.API.Enabled != true {
		t.Errorf("API.Enabled roundtrip: got %v, want true", loaded.API.Enabled)
	}
	if loaded.API.Port != 9090 {
		t.Errorf("API.Port roundtrip: got %d, want 9090", loaded.API.Port)
	}
	if loaded.Memory.RepoPath != cfg.Memory.RepoPath {
		t.Errorf("Memory.RepoPath roundtrip: got %q, want %q", loaded.Memory.RepoPath, cfg.Memory.RepoPath)
	}
}

func TestSave_NilRejected(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Save(nil); err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.UI.Port != 3000 {
		t.Errorf("expected default UI port 3000, got %d", d.UI.Port)
	}
	if d.Queue.MaxDepth != 8 {
		t.Errorf("expected default queue depth 8, got %d", d.Queue.MaxDepth)
	}
	if d.Metrics.RetentionDays != 30 {
		t.Errorf("expected default retention 30, got %d", d.Metrics.RetentionDays)
	}
}

func TestValidate(t *testing.T) {
	cfg := Defaults()
	if err := Validate(&cfg); err == nil {
		t.Error("expected Validate to fail on empty defaults (missing required paths)")
	}
	cfg.Model.Binary = "/x"
	cfg.Model.ModelPath = "/y.gguf"
	cfg.Embedder.Binary = "/a"
	cfg.Embedder.ModelPath = "/b.gguf"
	if err := Validate(&cfg); err != nil {
		t.Errorf("expected Validate to pass once required fields set: %v", err)
	}
}
