package ui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
)

func newServerWithMetricsStore(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "harness.db"), testDefaultMemoryRepoPath(dir))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := NewServer(3000)
	s.SetConfigStore(d.Config())
	s.SetMetricsStore(d.Metrics())
	return s, d
}

func TestHandleMetrics_DisabledReturnsNotFound(t *testing.T) {
	s, _ := newServerWithMetricsStore(t)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleMetrics_ExportsLatestPrometheusSamples(t *testing.T) {
	s, d := newServerWithMetricsStore(t)
	cfg := config.Defaults()
	cfg.Model.Binary = "C:\\llama.exe"
	cfg.Model.ModelPath = "C:\\model.gguf"
	cfg.Embedder.Binary = "C:\\embed.exe"
	cfg.Embedder.ModelPath = "C:\\embed.gguf"
	cfg.Metrics.PrometheusEnabled = true
	if err := d.Config().Save(&cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if err := d.Metrics().Record("queue_depth", 2, nil); err != nil {
		t.Fatalf("record queue depth: %v", err)
	}
	if err := d.Metrics().Record("process_health", 1, map[string]string{"process": "llama-server"}); err != nil {
		t.Fatalf("record process health: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE harness_queue_depth gauge",
		"harness_queue_depth 2",
		`harness_process_health{process="llama-server"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}
