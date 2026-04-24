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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/db"
	"github.com/vrnc/harness/internal/logbuf"
)

// newServerWithStore returns a Server wired to a fresh temp SQLite config store.
// The store is also returned for assertions.
func newServerWithStore(t *testing.T) (*Server, config.Store) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	store := d.Config()
	s := NewServer(3000)
	s.SetConfigStore(store)
	return s, store
}

func TestHandleStatus_OK(t *testing.T) {
	s := NewServer(3000)

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
	s := NewServer(3000)
	s.AddStartupError(errors.New("llama-server binary not found: C:\\missing.exe"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "llama-server binary not found") {
		t.Error("expected startup error in response body")
	}
}

func TestHandleStatus_FirstRunShowsSetupCTA(t *testing.T) {
	s := NewServer(3000)
	s.SetFirstRun(true)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Set up your harness") {
		t.Error("expected first-run CTA when FirstRun=true")
	}
	if !strings.Contains(string(body), "/config") {
		t.Error("expected CTA to link to /config")
	}
}

func TestSetLlamaStatus(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama", Running: true, Healthy: true})

	if !s.state.snapshot().LlamaStatus.Healthy {
		t.Error("expected llama status healthy")
	}
}

func TestSetQueueDepth(t *testing.T) {
	s := NewServer(3000)
	s.SetQueueDepth(3, 8)

	snap := s.state.snapshot()
	if snap.QueueDepth != 3 || snap.QueueMax != 8 {
		t.Errorf("expected depth 3/8, got %d/%d", snap.QueueDepth, snap.QueueMax)
	}
}

func TestHandleConfig_GETRendersFormWithDefaults(t *testing.T) {
	s, _ := newServerWithStore(t)

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
		t.Error("expected first-run banner when config has never been saved")
	}
}

func TestHandleConfig_GETWithoutStoreShowsError(t *testing.T) {
	s := NewServer(3000) // no store attached

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "config store unavailable") {
		t.Error("expected 'config store unavailable' message when no store is attached")
	}
}

func TestHandleConfig_POSTSavesAndRedirects(t *testing.T) {
	s, store := newServerWithStore(t)

	var retryCalls int32
	s.SetRetry(func() ApplyResult { atomic.AddInt32(&retryCalls, 1); return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_ctx_size", "8192")
	form.Set("model_gpu_layers", "20")
	form.Set("model_n_parallel", "1")
	form.Set("model_port", "8081")
	form.Set("model_verbose", "on")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("embed_verbose", "on")
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

	loaded, configured, err := store.Load()
	if err != nil {
		t.Fatalf("loading saved config failed: %v", err)
	}
	if !configured {
		t.Error("expected configured=true after POST")
	}
	if loaded.Model.Binary != "C:\\llama.exe" {
		t.Errorf("model binary not persisted: got %q", loaded.Model.Binary)
	}
	if !loaded.Model.Verbose {
		t.Error("expected Model.Verbose=true after POST with model_verbose=on")
	}
	if !loaded.Embedder.Verbose {
		t.Error("expected Embedder.Verbose=true after POST with embed_verbose=on")
	}

	if atomic.LoadInt32(&retryCalls) != 1 {
		t.Errorf("expected retry callback to fire once, got %d", retryCalls)
	}
}

// A subsequent POST without the verbose checkboxes must clear them - HTML
// forms omit unchecked checkboxes entirely, so missing value means false.
func TestHandleConfig_POSTClearsVerboseWhenUnchecked(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	// Seed with verbose=true.
	seed := config.Defaults()
	seed.Model.Binary = "C:\\llama.exe"
	seed.Model.ModelPath = "C:\\m.gguf"
	seed.Model.Verbose = true
	seed.Embedder.Binary = "C:\\embed.exe"
	seed.Embedder.ModelPath = "C:\\e.gguf"
	seed.Embedder.Verbose = true
	if err := store.Save(&seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_port", "8081")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")
	// Deliberately omit model_verbose and embed_verbose.

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Model.Verbose {
		t.Error("expected Model.Verbose=false when form omitted the checkbox")
	}
	if loaded.Embedder.Verbose {
		t.Error("expected Embedder.Verbose=false when form omitted the checkbox")
	}
}

func TestHandleConfig_POSTIncludesApplyResultInRedirect(t *testing.T) {
	s, _ := newServerWithStore(t)
	s.SetRetry(func() ApplyResult {
		return ApplyResult{
			LiveApplied:   true,
			RestartNeeded: []string{"UI port", "queue max depth"},
		}
	})

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_port", "8081")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/config?saved=1&applied=1&restart=UI+port%7Cqueue+max+depth"
	if loc != want {
		t.Errorf("redirect mismatch:\n got: %q\nwant: %q", loc, want)
	}
}

func TestHandleConfig_GETParsesApplyResultFromQuery(t *testing.T) {
	s, store := newServerWithStore(t)
	// Pre-seed so renderConfig has something to render.
	_ = store.Save(&config.Config{
		Model:    config.ModelConfig{Binary: "x", ModelPath: "y", Port: 1},
		Embedder: config.EmbedderConfig{Binary: "x", ModelPath: "y", Port: 2},
		UI:       config.UIConfig{Port: 3},
	})

	req := httptest.NewRequest(http.MethodGet, "/config?saved=1&applied=1&restart=UI+port%7Cqueue+max+depth", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Model and embedder are reloading live") {
		t.Errorf("expected mixed-apply message in body, got:\n%s", body)
	}
	if !strings.Contains(body, "UI port") || !strings.Contains(body, "queue max depth") {
		t.Errorf("expected restart reasons in body, got:\n%s", body)
	}
}

func TestHandleConfig_POSTPreservesExistingNumericsWhenBlank(t *testing.T) {
	s, store := newServerWithStore(t)

	// Seed store with a config whose numeric values diverge from Defaults.
	existing := config.Defaults()
	existing.Model.Binary = "C:\\existing.exe"
	existing.Model.ModelPath = "C:\\existing.gguf"
	existing.Model.CtxSize = 11111
	existing.Model.GPULayers = 42
	existing.Embedder.Binary = "C:\\eb.exe"
	existing.Embedder.ModelPath = "C:\\eb.gguf"
	existing.Prompt.MemoryTokenBudget = 9999
	if err := store.Save(&existing); err != nil {
		t.Fatalf("seed save: %v", err)
	}

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

	loaded, _, err := store.Load()
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

func TestHandleConfig_POSTPersistsLogBufferFields(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("log_ring_max_entries", "1500")
	form.Set("log_proc_max_lines", "200")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Log.RingMaxEntries != 1500 {
		t.Errorf("RingMaxEntries: got %d, want 1500", loaded.Log.RingMaxEntries)
	}
	if loaded.Log.ProcMaxLines != 200 {
		t.Errorf("ProcMaxLines: got %d, want 200", loaded.Log.ProcMaxLines)
	}
}

func TestHandleConfig_GETRendersLogFields(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, want := range []string{`name="log_ring_max_entries"`, `name="log_proc_max_lines"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected config form to include %q", want)
		}
	}
}

func TestHandleConfig_POSTInvalidShowsValidationError(t *testing.T) {
	s, store := newServerWithStore(t)

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

	_, configured, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if configured {
		t.Error("config should not be marked configured when validation fails")
	}
}

func TestHandleConfig_GETRendersDatalistAnchors(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, id := range []string{
		"model_binary_options",
		"model_path_options",
		"embed_binary_options",
		"embed_path_options",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("expected datalist with id=%q", id)
		}
	}
}

func TestHandleConfig_GETPreFillsDetectedLlamaBinary(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}
	want := filepath.Join(dir, exe)
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.SetBinDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, want) {
		t.Errorf("expected detected llama-server %q to appear in rendered form", want)
	}
}

func TestHandleConfig_GETOffersModelSuggestionsInDatalist(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.Mkdir(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir models: %v", err)
	}
	main := filepath.Join(modelsDir, "Qwen3-35B.gguf")
	embed := filepath.Join(modelsDir, "nomic-embed-v2.gguf")
	for _, p := range []string{main, embed} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	s.SetBinDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `value="`+main+`"`) {
		t.Errorf("expected main model option for %s in rendered body", main)
	}
	if !strings.Contains(body, `value="`+embed+`"`) {
		t.Errorf("expected embedder model option for %s in rendered body", embed)
	}
}

func TestHandleConfig_POSTErrorDoesNotPreFillBinary(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}
	detected := filepath.Join(dir, exe)
	if err := os.WriteFile(detected, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.SetBinDir(dir)

	// POST with blank required fields → re-renders with ValidationErr. The
	// rendered form should echo the user's submission (empty) rather than
	// silently inserting the detected path.
	form := url.Values{}
	form.Set("ui_port", "3000")
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="model_binary" value="`+detected+`"`) {
		t.Error("POST error re-render should not overwrite the user's submitted value with detected path")
	}
}

func TestHandleRetry_CallsCallback(t *testing.T) {
	s := NewServer(3000)
	var called int32
	s.SetRetry(func() ApplyResult { atomic.AddInt32(&called, 1); return ApplyResult{} })

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
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/retry", nil)
	rec := httptest.NewRecorder()
	s.handleRetry(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleProcRestart_CallsMatchingCallback(t *testing.T) {
	s := NewServer(3000)
	var llama, embed int32
	s.SetProcRestarts(
		func() { atomic.AddInt32(&llama, 1) },
		func() { atomic.AddInt32(&embed, 1) },
	)

	req := httptest.NewRequest(http.MethodPost, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("llama: expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&llama) != 1 || atomic.LoadInt32(&embed) != 0 {
		t.Errorf("expected llama callback only, llama=%d embed=%d", llama, embed)
	}

	req = httptest.NewRequest(http.MethodPost, "/procs/embed/restart", nil)
	rec = httptest.NewRecorder()
	s.handleProcRestart("embed")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("embed: expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&embed) != 1 {
		t.Errorf("expected embed callback to be called, got %d", embed)
	}
}

func TestHandleProcRestart_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleProcRestart_NoCallbackStillRedirects(t *testing.T) {
	// The manager may not be up yet on first run. The handler must not
	// panic and must still redirect so the UI doesn't show a blank page.
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodPost, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected 303 even without callback, got %d", rec.Code)
	}
}

func TestHandleStatus_RendersRestartFormWhenFailed(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama-server", Failed: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `action="/procs/llama/restart"`) {
		t.Error("status page should include llama restart form when Failed")
	}
	// The restart form must be visible (no hidden attribute) when Failed.
	if !strings.Contains(body, `id="llama-restart-form"`) {
		t.Fatal("llama restart form missing from body")
	}
	if strings.Contains(body, `id="llama-restart-form" hidden`) ||
		strings.Contains(body, `hidden id="llama-restart-form"`) {
		t.Error("llama restart form should not be hidden when Failed=true")
	}
	// Badge text can have surrounding whitespace from the template; just
	// verify the word appears and the other two options do not.
	if !strings.Contains(body, "Failed") {
		t.Error("badge should read 'Failed' when Status.Failed is true")
	}
	// When Failed=true the badge must not simultaneously render the other
	// two states. Look for them between the open tag and its close.
	const open = `id="llama-badge"`
	i := strings.Index(body, open)
	j := strings.Index(body[i:], "</span>")
	if i < 0 || j < 0 {
		t.Fatal("could not locate llama-badge span in rendered body")
	}
	badge := body[i : i+j]
	if strings.Contains(badge, "Healthy") || strings.Contains(badge, "Unhealthy") {
		t.Errorf("badge should not render Healthy/Unhealthy when Failed; got %q", badge)
	}
}

func TestHandleStatus_HidesRestartFormWhenNotFailed(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama-server", Running: true, Healthy: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `id="llama-restart-form"`) {
		t.Fatal("llama restart form should still be in DOM so JS can toggle it")
	}
	if !strings.Contains(body, `hidden`) {
		t.Error("llama restart form should be hidden when not Failed")
	}
}

func TestHandleStatus_RendersRecentLogs(t *testing.T) {
	s := NewServer(3000)
	ring := logbuf.New(10)
	s.SetLogRing(ring)
	if _, err := ring.Write([]byte("hello world\nsecond line\n")); err != nil {
		t.Fatalf("ring write: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"hello world", "second line"} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing log line %q", want)
		}
	}
}

func TestHandleStatus_NoLogRingRendersEmpty(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestStreamRing_NoRingReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/logs/harness", nil)
	rec := httptest.NewRecorder()
	s.streamRing(s.getLogRing)(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no ring, got %d", rec.Code)
	}
}

// flushRecorder is a ResponseRecorder that satisfies http.Flusher so streaming
// handlers don't bail out on the type assertion. Flush is a no-op because the
// recorder always has the bytes available immediately.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

func TestStreamRing_StreamsNewEntries(t *testing.T) {
	s := NewServer(3000)
	ring := logbuf.New(10)
	s.SetLogRing(ring)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/logs/harness", nil).WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		s.streamRing(s.getLogRing)(rec, req)
		close(done)
	}()

	// Give the handler time to register its subscription before we publish.
	time.Sleep(50 * time.Millisecond)
	if _, err := ring.Write([]byte("hello sse\n")); err != nil {
		t.Fatalf("ring write: %v", err)
	}
	// Allow the fan-out + write to land before we tear down.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "hello sse") {
		t.Errorf("SSE payload missing line, got: %q", body)
	}
	// The stream opens with a ": connected" comment so headers flush
	// immediately and the browser fires onopen; the real entry follows.
	if !strings.HasPrefix(body, ": connected\n\n") {
		t.Errorf("SSE payload did not begin with connected comment, got: %q", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Errorf("SSE payload missing data frame, got: %q", body)
	}
}

func TestStart_ServerStarts(t *testing.T) {
	s := NewServer(13001) // use a high port to avoid conflicts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

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
