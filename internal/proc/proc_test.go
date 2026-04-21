package proc

import (
	"testing"
	"time"
)

func TestStatus_InitialState(t *testing.T) {
	events := make(chan Event, 10)
	m := NewManager(ManagerConfig{
		Name: "test",
		BuildArgs: func() (string, []string) {
			return "nonexistent-binary", nil
		},
		HealthURL:   "http://127.0.0.1:9999/health",
		Events:      events,
		CheckPeriod: 5 * time.Second,
	})

	s := m.Status()
	if s.Running {
		t.Error("expected not running initially")
	}
	if s.Healthy {
		t.Error("expected not healthy initially")
	}
	if s.RestartCount != 0 {
		t.Errorf("expected restart count 0, got %d", s.RestartCount)
	}
	if s.LastError != nil {
		t.Errorf("expected nil LastError initially, got %v", s.LastError)
	}
}

func TestLlamaArgs(t *testing.T) {
	bin, args := LlamaArgs("/bin/llama-server", "/models/model.gguf", 4096, 10, 2, 8081)
	if bin != "/bin/llama-server" {
		t.Errorf("unexpected binary: %s", bin)
	}
	found := map[string]bool{}
	for i := 0; i < len(args)-1; i++ {
		found[args[i]] = true
	}
	for _, flag := range []string{"--model", "--ctx-size", "--n-gpu-layers", "--parallel", "--port", "--host"} {
		if !found[flag] {
			t.Errorf("missing flag %s in args: %v", flag, args)
		}
	}
}

func TestEmbedderArgs(t *testing.T) {
	bin, args := EmbedderArgs("/bin/embedder", "/models/embed.gguf", 8082)
	if bin != "/bin/embedder" {
		t.Errorf("unexpected binary: %s", bin)
	}
	if len(args) == 0 {
		t.Error("expected non-empty args for embedder")
	}
}

func TestEventKindConstants(t *testing.T) {
	kinds := []EventKind{EventStart, EventStop, EventHealthOK, EventHealthFail, EventRestart, EventError}
	seen := map[EventKind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("duplicate EventKind: %q", k)
		}
		seen[k] = true
		if k == "" {
			t.Error("empty EventKind")
		}
	}
}
