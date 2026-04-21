package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vrnc/harness/internal/config"
)

func TestHandleStatus_OK(t *testing.T) {
	s := NewServer(3000, "")

	// Use httptest recorder directly instead of starting a real server.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Harness") {
		t.Error("expected 'Harness' in response body")
	}
}

func TestHandleStatus_WithErrors(t *testing.T) {
	s := NewServer(3000, "")
	s.AddStartupError(errors.New("config.toml not found"))
	s.AddStartupError(errors.New("llama-server binary not found"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "config.toml not found") {
		t.Error("expected startup error in response body")
	}
}

func TestSetLlamaStatus(t *testing.T) {
	s := NewServer(3000, "")
	s.SetLlamaStatus(ProcessStatus{Name: "llama", Running: true, Healthy: true})

	healthy := s.state.snapshot().LlamaStatus.Healthy

	if !healthy {
		t.Error("expected llama status healthy")
	}
}

func TestSetQueueDepth(t *testing.T) {
	s := NewServer(3000, "")
	s.SetQueueDepth(3, 8)

	snap := s.state.snapshot()
	depth := snap.QueueDepth
	max := snap.QueueMax

	if depth != 3 || max != 8 {
		t.Errorf("expected depth 3/8, got %d/%d", depth, max)
	}
}

func TestHandleStatus_ConfigMissingShowsFirstRunCTA(t *testing.T) {
	s := NewServer(3000, "")
	// Matches the prefix produced by config.Load when config.toml is missing.
	s.AddStartupError(errors.New("config: config.toml not found at C:\\harness\\config.toml"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Set up your harness") {
		t.Error("expected first-run CTA for config-missing error")
	}
	if !strings.Contains(string(body), "/config") {
		t.Error("expected CTA to link to /config")
	}
}

func TestHandleConfig_GETRendersFormWithDefaults(t *testing.T) {
	// No config on disk → form renders with Defaults() values.
	dir := t.TempDir()
	s := NewServer(3000, dir)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="model_binary"`) {
		t.Error("expected form field for model binary")
	}
	if !strings.Contains(body, "First run") {
		t.Error("expected first-run banner when config is absent")
	}
}

func TestHandleConfig_POSTSavesAndRedirects(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(3000, dir)

	var retryCalls int32
	s.SetRetry(func() { atomic.AddInt32(&retryCalls, 1) })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_ctx_size", "8192")
	form.Set("model_gpu_layers", "20")
	form.Set("model_n_parallel", "1")
	form.Set("model_port", "8081")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")
	form.Set("ui_open_on_start", "on")
	form.Set("api_port", "8080")
	form.Set("prompt_ctx_size", "8192")
	form.Set("prompt_memory_budget", "2048")
	form.Set("prompt_conversation_reserve", "4096")
	form.Set("queue_max_depth", "8")
	form.Set("metrics_retention_days", "30")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/config?saved=1" {
		t.Errorf("expected redirect to /config?saved=1, got %q", got)
	}

	// File must exist and round-trip through Load.
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("expected config.toml to be written: %v", err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("loading saved config failed: %v", err)
	}
	if loaded.Model.Binary != "C:\\llama.exe" {
		t.Errorf("model binary not persisted: got %q", loaded.Model.Binary)
	}

	if atomic.LoadInt32(&retryCalls) != 1 {
		t.Errorf("expected retry callback to fire once, got %d", retryCalls)
	}
}

func TestHandleConfig_POSTPreservesExistingNumericsWhenBlank(t *testing.T) {
	dir := t.TempDir()
	// Seed disk with a config whose numeric values diverge from Defaults.
	existing := config.Defaults()
	existing.Model.Binary = "C:\\existing.exe"
	existing.Model.ModelPath = "C:\\existing.gguf"
	existing.Model.CtxSize = 11111
	existing.Model.GPULayers = 42
	existing.Embedder.Binary = "C:\\eb.exe"
	existing.Embedder.ModelPath = "C:\\eb.gguf"
	existing.Prompt.MemoryTokenBudget = 9999
	if err := config.Save(&existing, dir); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	s := NewServer(3000, dir)

	form := url.Values{}
	// Update only the string fields; leave every numeric field blank.
	form.Set("model_binary", "C:\\new.exe")
	form.Set("model_path", "C:\\new.gguf")
	form.Set("embed_binary", "C:\\eb.exe")
	form.Set("embed_path", "C:\\eb.gguf")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}

	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if loaded.Model.CtxSize != 11111 {
		t.Errorf("expected Model.CtxSize preserved (11111), got %d", loaded.Model.CtxSize)
	}
	if loaded.Model.GPULayers != 42 {
		t.Errorf("expected Model.GPULayers preserved (42), got %d", loaded.Model.GPULayers)
	}
	if loaded.Prompt.MemoryTokenBudget != 9999 {
		t.Errorf("expected Prompt.MemoryTokenBudget preserved (9999), got %d", loaded.Prompt.MemoryTokenBudget)
	}
	if loaded.Model.Binary != "C:\\new.exe" {
		t.Errorf("expected Model.Binary updated, got %q", loaded.Model.Binary)
	}
}

func TestHandleConfig_POSTInvalidShowsValidationError(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(3000, dir)

	form := url.Values{}
	// Deliberately omit model_binary, model_path, embed_binary, embed_path.
	form.Set("ui_port", "3000")
	form.Set("model_port", "8081")
	form.Set("embed_port", "8082")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render with error), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Validation error") {
		t.Error("expected validation error message in rendered form")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); !os.IsNotExist(err) {
		t.Error("config.toml should not be written when validation fails")
	}
}

func TestHandleRetry_CallsCallback(t *testing.T) {
	s := NewServer(3000, "")
	var called int32
	s.SetRetry(func() { atomic.AddInt32(&called, 1) })

	req := httptest.NewRequest(http.MethodPost, "/retry", nil)
	rec := httptest.NewRecorder()
	s.handleRetry(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Errorf("expected retry to be called once, got %d", called)
	}
}

func TestHandleRetry_RejectsGET(t *testing.T) {
	s := NewServer(3000, "")
	req := httptest.NewRequest(http.MethodGet, "/retry", nil)
	rec := httptest.NewRecorder()
	s.handleRetry(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestStart_ServerStarts(t *testing.T) {
	s := NewServer(13001, "") // use a high port to avoid conflicts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// Give it a moment to bind.
	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		resp, err = http.Get("http://localhost:13001/")
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("could not connect to UI server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}
