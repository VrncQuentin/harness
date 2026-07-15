package db

import (
	"database/sql"
	"path/filepath"
	"strings"
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

	if err := d.sqldb.QueryRow("SELECT COUNT(*) FROM projects WHERE slug = 'global'").Scan(&count); err != nil {
		t.Fatalf("count global project rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 global project row after Open, got %d", count)
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
	if err := d2.sqldb.QueryRow("SELECT COUNT(*) FROM projects WHERE slug = 'global'").Scan(&count); err != nil {
		t.Fatalf("count global project rows: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 global project row after reopen, got %d", count)
	}
}

func TestOpen_RejectsUnexpectedMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.db")

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sqldb, err := sql.Open("sqlite", foreignKeysDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := sqldb.Exec(`UPDATE schema_migrations SET version = ?, dirty = ?`, 16, false); err != nil {
		t.Fatalf("set migration version: %v", err)
	}
	if err := sqldb.Close(); err != nil {
		t.Fatalf("sql Close: %v", err)
	}

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected migration version mismatch error")
	}
	if !strings.Contains(err.Error(), "migration version 16") {
		t.Fatalf("error %q does not mention recorded version", err)
	}
	if !strings.Contains(err.Error(), "delete harness.db and restart") {
		t.Fatalf("error %q does not tell the user how to recover", err)
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
	if cfg.Prompt.RecencyN != defaults.Prompt.RecencyN {
		t.Errorf("Prompt.RecencyN default: got %d, want %d", cfg.Prompt.RecencyN, defaults.Prompt.RecencyN)
	}
	if cfg.Prompt.SummarizerPrompt != defaults.Prompt.SummarizerPrompt {
		t.Errorf("Prompt.SummarizerPrompt default: got %q, want %q", cfg.Prompt.SummarizerPrompt, defaults.Prompt.SummarizerPrompt)
	}
	if cfg.Prompt.SummarizerPrompt == "" {
		t.Error("Prompt.SummarizerPrompt default must not be empty")
	}
	if cfg.Model.CacheTypeK != defaults.Model.CacheTypeK {
		t.Errorf("Model.CacheTypeK default: got %q, want %q", cfg.Model.CacheTypeK, defaults.Model.CacheTypeK)
	}
	if cfg.Model.CacheTypeV != defaults.Model.CacheTypeV {
		t.Errorf("Model.CacheTypeV default: got %q, want %q", cfg.Model.CacheTypeV, defaults.Model.CacheTypeV)
	}
	if cfg.Project.ActiveProjectSlug != defaults.Project.ActiveProjectSlug {
		t.Errorf("Project.ActiveProjectSlug default: got %q, want %q", cfg.Project.ActiveProjectSlug, defaults.Project.ActiveProjectSlug)
	}
	if cfg.Project.LlamaOnSwitch != defaults.Project.LlamaOnSwitch {
		t.Errorf("Project.LlamaOnSwitch default: got %q, want %q", cfg.Project.LlamaOnSwitch, defaults.Project.LlamaOnSwitch)
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
	cfg.Model.CacheTypeK = "q4_0"
	cfg.Model.CacheTypeV = "f16"
	cfg.Embedder.Binary = "C:\\embed.exe"
	cfg.Embedder.ModelPath = "C:\\e.gguf"
	cfg.Embedder.Verbose = true
	cfg.Agent.Active = "coder"
	cfg.UI.OpenOnStart = false
	cfg.API.Enabled = true
	cfg.API.Port = 9090
	cfg.Prompt.RecencyN = 13
	cfg.Prompt.SummarizerPrompt = "summarize the conversation as one short paragraph."
	cfg.Log.RingMaxEntries = 1234
	cfg.Log.ProcMaxLines = 99
	cfg.Project.ActiveProjectSlug = "dt"
	cfg.Project.LlamaOnSwitch = "keep"

	// insert the project so the active-project referential trigger allows the save
	_, err := d.sqldb.Exec(
		"INSERT INTO projects (slug, display_name, hidden, created_at) VALUES (?, ?, ?, ?)",
		"dt", "DT", 0, time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insert dt project: %v", err)
	}

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
	if loaded.Agent.Active != "coder" {
		t.Errorf("Agent.Active roundtrip: got %q, want %q", loaded.Agent.Active, "coder")
	}
	if loaded.Prompt.RecencyN != 13 {
		t.Errorf("Prompt.RecencyN roundtrip: got %d, want 13", loaded.Prompt.RecencyN)
	}
	if loaded.Prompt.SummarizerPrompt != cfg.Prompt.SummarizerPrompt {
		t.Errorf("Prompt.SummarizerPrompt roundtrip: got %q, want %q", loaded.Prompt.SummarizerPrompt, cfg.Prompt.SummarizerPrompt)
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
	if loaded.Model.CacheTypeK != "q4_0" {
		t.Errorf("Model.CacheTypeK roundtrip: got %q, want %q", loaded.Model.CacheTypeK, "q4_0")
	}
	if loaded.Model.CacheTypeV != "f16" {
		t.Errorf("Model.CacheTypeV roundtrip: got %q, want %q", loaded.Model.CacheTypeV, "f16")
	}
	if loaded.Project.ActiveProjectSlug != "dt" {
		t.Errorf("Project.ActiveProjectSlug roundtrip: got %q, want %q", loaded.Project.ActiveProjectSlug, "dt")
	}
	if loaded.Project.LlamaOnSwitch != "keep" {
		t.Errorf("Project.LlamaOnSwitch roundtrip: got %q, want %q", loaded.Project.LlamaOnSwitch, "keep")
	}
}

func TestConfigStore_SaveNilRejected(t *testing.T) {
	d := newTestDB(t)
	if err := d.Config().Save(nil); err == nil {
		t.Fatal("expected error for nil config, got nil")
	}
}

func TestSchema_ForeignKeysEnabled(t *testing.T) {
	d := newTestDB(t)

	_, err := d.sqldb.Exec(
		"INSERT INTO project_directories (project_slug, path) VALUES (?, ?)",
		"nonexistent", "/tmp/orphan",
	)
	if err == nil {
		t.Fatal("expected foreign-key error inserting orphan project_directories row, got nil")
	}
}

func TestSchema_ProtectGlobalProject(t *testing.T) {
	d := newTestDB(t)

	_, err := d.sqldb.Exec("DELETE FROM projects WHERE slug = 'global'")
	if err == nil {
		t.Error("expected error deleting global project, got nil")
	}

	_, err = d.sqldb.Exec("UPDATE projects SET slug = 'other' WHERE slug = 'global'")
	if err == nil {
		t.Error("expected error renaming global project slug, got nil")
	}

	_, err = d.sqldb.Exec("UPDATE projects SET hidden = 1 WHERE slug = 'global'")
	if err == nil {
		t.Error("expected error hiding global project, got nil")
	}

	_, err = d.sqldb.Exec("UPDATE projects SET display_name = 'Global Updated' WHERE slug = 'global'")
	if err != nil {
		t.Errorf("unexpected error updating global display_name: %v", err)
	}
}

func TestSchema_ProtectActiveProjectDelete(t *testing.T) {
	d := newTestDB(t)

	_, err := d.sqldb.Exec(
		"INSERT INTO projects (slug, display_name, hidden, created_at) VALUES (?, ?, ?, ?), (?, ?, ?, ?)",
		"alpha", "Alpha", 0, time.Now().Unix(),
		"beta", "Beta", 0, time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insert projects: %v", err)
	}

	_, err = d.sqldb.Exec("UPDATE config SET active_project_slug = 'alpha' WHERE id = 1")
	if err != nil {
		t.Fatalf("set active project to alpha: %v", err)
	}

	_, err = d.sqldb.Exec("DELETE FROM projects WHERE slug = 'alpha'")
	if err == nil {
		t.Error("expected error deleting active project, got nil")
	}

	_, err = d.sqldb.Exec("DELETE FROM projects WHERE slug = 'beta'")
	if err != nil {
		t.Errorf("unexpected error deleting non-active project: %v", err)
	}
}

func TestSchema_ActiveProjectSlugReferentialIntegrity(t *testing.T) {
	d := newTestDB(t)
	store := d.Config()

	_, err := d.sqldb.Exec(
		"INSERT INTO projects (slug, display_name, hidden, created_at) VALUES (?, ?, ?, ?)",
		"dt", "DT", 0, time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insert dt project: %v", err)
	}

	cfg, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Project.ActiveProjectSlug = "dt"
	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save with existing dt project: %v", err)
	}

	cfg.Project.ActiveProjectSlug = "missing"
	if err := store.Save(cfg); err == nil {
		t.Error("expected error saving config with missing active_project_slug, got nil")
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

func TestMetricsStore_ApplyRetentionDownsamplesAndPrunes(t *testing.T) {
	d := newTestDB(t)
	store := d.Metrics()

	now := time.Now()
	oldHour := now.Add(-31 * 24 * time.Hour).Truncate(time.Hour)
	recent := now.Add(-time.Hour)
	_, err := d.sqldb.Exec(
		`INSERT INTO metrics(name, value, tags, ts) VALUES
			(?, ?, ?, ?),
			(?, ?, ?, ?),
			(?, ?, ?, ?)`,
		"queue_depth", 10.0, "", oldHour.Add(5*time.Minute).Unix(),
		"queue_depth", 20.0, "", oldHour.Add(20*time.Minute).Unix(),
		"queue_depth", 5.0, "", recent.Unix(),
	)
	if err != nil {
		t.Fatalf("insert metrics: %v", err)
	}

	if err := store.ApplyRetention(30); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}

	var rawOld int
	if err := d.sqldb.QueryRow(`SELECT COUNT(*) FROM metrics WHERE ts < ?`, now.Add(-30*24*time.Hour).Unix()).Scan(&rawOld); err != nil {
		t.Fatalf("count old raw: %v", err)
	}
	if rawOld != 0 {
		t.Fatalf("old raw metric rows = %d, want 0", rawOld)
	}

	var count int
	var minValue, maxValue, avgValue, lastValue float64
	if err := d.sqldb.QueryRow(
		`SELECT count, min_value, max_value, avg_value, last_value FROM metrics_hourly WHERE name = ? AND hour_ts = ?`,
		"queue_depth", oldHour.Unix(),
	).Scan(&count, &minValue, &maxValue, &avgValue, &lastValue); err != nil {
		t.Fatalf("query hourly aggregate: %v", err)
	}
	if count != 2 || minValue != 10 || maxValue != 20 || avgValue != 15 || lastValue != 20 {
		t.Fatalf("hourly aggregate = count %d min %v max %v avg %v last %v", count, minValue, maxValue, avgValue, lastValue)
	}

	pts, err := store.Query("queue_depth", oldHour.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("points = %d, want hourly + recent raw", len(pts))
	}
	if pts[0].Time.Unix() != oldHour.Unix() || pts[0].Value != 15 {
		t.Fatalf("old point = %+v, want hourly average at %v", pts[0], oldHour)
	}
	if pts[1].Value != 5 {
		t.Fatalf("recent raw value = %v, want 5", pts[1].Value)
	}
}

func TestMetricsStore_LatestUsesHourlyWhenRawWasPruned(t *testing.T) {
	d := newTestDB(t)
	store := d.Metrics()

	oldHour := time.Now().Add(-31 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := d.sqldb.Exec(
		`INSERT INTO metrics(name, value, tags, ts) VALUES (?, ?, ?, ?)`,
		"episode_count", 7.0, "", oldHour.Add(10*time.Minute).Unix(),
	); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	if err := store.ApplyRetention(30); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}

	pts, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("latest points = %d, want 1", len(pts))
	}
	if pts[0].Name != "episode_count" || pts[0].Value != 7 || pts[0].Time.Unix() != oldHour.Unix() {
		t.Fatalf("latest point = %+v", pts[0])
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
