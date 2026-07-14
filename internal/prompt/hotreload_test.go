package prompt

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type splitRepo struct {
	global string
	active string
}

func makeSplitRepo(t *testing.T, globalFiles, activeFiles map[string]string) splitRepo {
	t.Helper()
	root := t.TempDir()
	repo := splitRepo{
		global: filepath.Join(root, "global"),
		active: filepath.Join(root, "projects", "demo"),
	}
	writeFiles(t, repo.global, globalFiles)
	writeFiles(t, repo.active, activeFiles)
	return repo
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll root: %v", err)
	}
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
}

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

func TestHotReloadLayoutV2_EmitsOnGlobalChange(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{
		"rules.md": "start",
		"user.md":  "u",
		"facts.md": "f",
	}, nil)
	logger, buf := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(repo.global, "rules.md"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if !waitForLog(buf, "prompt: file changed") || !waitForLog(buf, "rules.md") {
		t.Errorf("expected rules.md change log, got:\n%s", buf.String())
	}
}

func TestHotReloadLayoutV2_IgnoresUnrelatedFile(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{"rules.md": "start"}, nil)
	logger, buf := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(repo.global, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(hotReloadDebounce * 2)
	if strings.Contains(buf.String(), "prompt: file changed") {
		t.Errorf("unexpected change event for untracked file:\n%s", buf.String())
	}
}

func TestHotReloadLayoutV2_DebounceCollapsesBurst(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{"rules.md": "start"}, nil)
	logger, buf := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)
	rules := filepath.Join(repo.global, "rules.md")
	for i := range 5 {
		if err := os.WriteFile(rules, []byte("v"+string(rune('0'+i))), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !waitForLog(buf, "prompt: file changed") {
		t.Fatalf("expected at least one change event, got:\n%s", buf.String())
	}
	time.Sleep(hotReloadDebounce * 2)
	count := strings.Count(buf.String(), `"msg":"prompt: file changed"`)
	if count < 1 || count > 2 {
		t.Errorf("expected 1-2 change events after debounce, got %d:\n%s", count, buf.String())
	}
}

func TestHotReloadLayoutV2_EmitsOnActiveProjectRulesChange(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{"rules.md": "global"}, map[string]string{"rules.md": "project"})
	logger, buf := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(repo.active, "rules.md"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !waitForLog(buf, "prompt: file changed") || !waitForLog(buf, "rules.md") {
		t.Errorf("expected project rules change log, got:\n%s", buf.String())
	}
}

func TestHotReloadLayoutV2_SetActiveAgentSwapsWatches(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{
		"agents/coder/persona.md":    "c",
		"agents/reviewer/persona.md": "r",
	}, map[string]string{
		"agents/coder/persona.md":    "pc",
		"agents/reviewer/persona.md": "pr",
	})
	logger, buf := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "coder", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	defer func() { _ = h.Close() }()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(repo.active, "agents", "coder", "persona.md"), []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !waitForLog(buf, "coder") {
		t.Fatalf("expected coder change, got:\n%s", buf.String())
	}

	time.Sleep(hotReloadDebounce * 2)
	preSwapLen := len(buf.String())
	h.SetActiveAgent("reviewer")
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(repo.active, "agents", "reviewer", "persona.md"), []byte("updated"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !waitForLog(buf, "reviewer") {
		t.Errorf("expected reviewer change, got:\n%s", buf.String()[preSwapLen:])
	}

	time.Sleep(hotReloadDebounce * 2)
	preStaleLen := len(buf.String())
	if err := os.WriteFile(filepath.Join(repo.active, "agents", "coder", "persona.md"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(hotReloadDebounce * 2)
	if strings.Contains(buf.String()[preStaleLen:], "prompt: file changed") {
		t.Errorf("expected stale coder change to be ignored after swap, got:\n%s", buf.String()[preStaleLen:])
	}
}

func TestHotReloadLayoutV2_MissingOptionalDirsDoNotFail(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{"rules.md": "r"}, nil)
	logger, _ := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "ghost", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2 should succeed with missing optional dirs: %v", err)
	}
	defer func() { _ = h.Close() }()
}

func TestHotReloadLayoutV2_CloseIsIdempotent(t *testing.T) {
	repo := makeSplitRepo(t, map[string]string{"rules.md": "r"}, nil)
	logger, _ := captureLogger(t)
	h, err := NewHotReloadLayoutV2(repo.global, repo.active, "", "demo", logger)
	if err != nil {
		t.Fatalf("NewHotReloadLayoutV2: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
