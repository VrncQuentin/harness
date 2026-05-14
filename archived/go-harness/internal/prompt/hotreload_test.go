package prompt

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeRepo scaffolds a memory repo under t.TempDir() and returns its
// absolute path.
func makeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return root
}

// captureLogger returns a slog.Logger that writes JSON into buf so
// tests can assert on the emitted entries. The mutex guards buf
// across the event pump goroutine and the asserting goroutine.
func captureLogger(t *testing.T) (*slog.Logger, *safeBuf) {
	t.Helper()
	buf := &safeBuf{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitForLog polls buf up to 2s for substring needle. Returns true on
// hit, false on timeout - fsnotify on Windows sometimes batches
// events, so generous margins reduce flakes.
func waitForLog(buf *safeBuf, needle string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), needle) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestHotReload_EmitsOnGlobalChange(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md": "start",
		"global/user.md":  "u",
		"global/facts.md": "f",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	defer func() { _ = h.Close() }()

	// Give the OS a beat to register the watch before we rewrite.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(root, "global", "rules.md"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitForLog(buf, "prompt: file changed") {
		t.Errorf("expected file-changed log entry, got:\n%s", buf.String())
	}
	if !waitForLog(buf, "rules.md") {
		t.Errorf("expected rules.md in log, got:\n%s", buf.String())
	}
}

func TestHotReload_IgnoresUnrelatedFile(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md": "start",
		"global/user.md":  "u",
		"global/facts.md": "f",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)

	// Create an unrelated file in the watched directory; should not
	// trigger a log.
	if err := os.WriteFile(filepath.Join(root, "global", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wait past the debounce window to be sure nothing slipped through.
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(buf.String(), "prompt: file changed") {
		t.Errorf("unexpected change event for untracked file:\n%s", buf.String())
	}
}

func TestHotReload_DebounceCollapsesBurst(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md": "start",
		"global/user.md":  "u",
		"global/facts.md": "f",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)

	// Rapid writes within the debounce window should produce a single
	// log entry.
	rules := filepath.Join(root, "global", "rules.md")
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(rules, []byte("v"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !waitForLog(buf, "prompt: file changed") {
		t.Fatalf("expected at least one change event, got:\n%s", buf.String())
	}
	// Give debounce timer time to drain.
	time.Sleep(hotReloadDebounce * 2)

	count := strings.Count(buf.String(), `"msg":"prompt: file changed"`)
	// We allow 1 or 2 entries: Windows sometimes splits bursts when
	// the OS event queue fills, but an event-per-write (5+) indicates
	// the debounce was not applied.
	if count < 1 || count > 2 {
		t.Errorf("expected 1-2 change events after debounce, got %d:\n%s", count, buf.String())
	}
}

func TestHotReload_EmitsOnAgentRulesChange(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md":         "r",
		"agents/coder/persona.md": "p",
		"agents/coder/rules.md":   "before",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "coder", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)

	rulesPath := filepath.Join(root, "agents", "coder", "rules.md")
	if err := os.WriteFile(rulesPath, []byte("after"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitForLog(buf, "prompt: file changed") {
		t.Fatalf("expected file-changed log entry, got:\n%s", buf.String())
	}
	if !waitForLog(buf, "rules.md") {
		t.Errorf("expected rules.md in log, got:\n%s", buf.String())
	}
}

func TestHotReload_SetActiveAgentSwapsWatches(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md":            "r",
		"global/user.md":             "u",
		"global/facts.md":            "f",
		"agents/coder/persona.md":    "c",
		"agents/reviewer/persona.md": "r2",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "coder", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)

	// Changing coder/persona.md should fire.
	if err := os.WriteFile(filepath.Join(root, "agents", "coder", "persona.md"), []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !waitForLog(buf, "coder") {
		t.Fatalf("expected coder persona change, got:\n%s", buf.String())
	}

	// Swap to reviewer, wait for debounces, reset buffer by checking
	// length.
	time.Sleep(hotReloadDebounce * 2)
	preSwapLen := len(buf.String())
	h.SetActiveAgent("reviewer")

	time.Sleep(50 * time.Millisecond)

	// Reviewer change should fire under the new active agent.
	if err := os.WriteFile(filepath.Join(root, "agents", "reviewer", "persona.md"), []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !waitForLog(buf, "reviewer") {
		t.Errorf("expected reviewer persona change, got:\n%s", buf.String()[preSwapLen:])
	}
}

func TestHotReload_MissingFilesAreWarnedNotFailed(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md": "r",
		// No user.md, no facts.md, no agent at all.
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReload(root, "ghost", logger)
	if err != nil {
		t.Fatalf("NewHotReload should succeed with missing optional files: %v", err)
	}
	defer func() { _ = h.Close() }()

	// We don't assert on the exact log content - the missing agent
	// directory is logged at debug level only - but the constructor
	// returning nil proves missing files are not fatal.
	_ = buf
}

func TestHotReload_CloseIsIdempotent(t *testing.T) {
	root := makeRepo(t, map[string]string{
		"global/rules.md": "r",
	})
	logger, _ := captureLogger(t)
	h, err := NewHotReload(root, "", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestHotReload_CloseStopsGoroutine(t *testing.T) {
	// Not a rigorous leak check - we just verify Close returns
	// promptly even when there's an in-flight debounce timer.
	root := makeRepo(t, map[string]string{
		"global/rules.md": "r",
	})
	logger, _ := captureLogger(t)
	h, err := NewHotReload(root, "", logger)
	if err != nil {
		t.Fatalf("NewHotReload: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	// Schedule a debounce then close before it fires.
	if err := os.WriteFile(filepath.Join(root, "global", "rules.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	start := time.Now()
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Close blocked for %v; should return promptly", elapsed)
	}
}

// TestHotReload_NonWindowsGuards gives us a way to short-circuit
// platform-sensitive assertions without gating the whole file.
func TestHotReload_NonWindowsGuards(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platform-specific watch semantics vary; the happy-path tests cover this")
	}
}
