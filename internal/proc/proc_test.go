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
	found := map[string]bool{}
	for _, a := range args {
		found[a] = true
	}
	for _, flag := range []string{"--model", "--embedding", "--port", "--host"} {
		if !found[flag] {
			t.Errorf("missing flag %s in args: %v", flag, args)
		}
	}
}

func TestReconfigure_SwapsArgsAndSignalsReload(t *testing.T) {
	events := make(chan Event, 10)
	m := NewManager(ManagerConfig{
		Name: "test",
		BuildArgs: func() (string, []string) {
			return "old-binary", []string{"--old"}
		},
		HealthURL:   "http://127.0.0.1:9999/old",
		Events:      events,
		CheckPeriod: 5 * time.Second,
	})

	m.Reconfigure(func() (string, []string) {
		return "new-binary", []string{"--new"}
	}, "http://127.0.0.1:9999/new")

	// Next startProcess should see the new values.
	m.mu.Lock()
	build := m.buildArgs
	url := m.healthURL
	m.mu.Unlock()

	bin, args := build()
	if bin != "new-binary" || len(args) != 1 || args[0] != "--new" {
		t.Errorf("buildArgs not swapped: got %s %v", bin, args)
	}
	if url != "http://127.0.0.1:9999/new" {
		t.Errorf("healthURL not swapped: got %s", url)
	}

	// Reload signal should be pending so the Run loop picks it up.
	select {
	case <-m.reloadCh:
	default:
		t.Error("expected reload signal to be pending")
	}
}

func TestReconfigure_CoalescesMultipleCalls(t *testing.T) {
	events := make(chan Event, 10)
	m := NewManager(ManagerConfig{
		Name: "test",
		BuildArgs: func() (string, []string) {
			return "binary", nil
		},
		HealthURL:   "http://127.0.0.1:9999/health",
		Events:      events,
		CheckPeriod: 5 * time.Second,
	})

	// Three calls back-to-back must not deadlock on the buffered channel.
	m.Reconfigure(func() (string, []string) { return "a", nil }, "http://a")
	m.Reconfigure(func() (string, []string) { return "b", nil }, "http://b")
	m.Reconfigure(func() (string, []string) { return "c", nil }, "http://c")

	// One signal should be pending (channel is buffered at 1).
	select {
	case <-m.reloadCh:
	default:
		t.Error("expected a reload signal")
	}
	// No second signal.
	select {
	case <-m.reloadCh:
		t.Error("expected exactly one pending reload signal")
	default:
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
