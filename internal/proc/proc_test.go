package proc

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	if s.Failed {
		t.Error("expected Failed=false initially")
	}
}

func TestHealthURL(t *testing.T) {
	if got := HealthURL(8123); got != "http://127.0.0.1:8123/health" {
		t.Fatalf("HealthURL = %q", got)
	}
}
func TestLlamaArgs(t *testing.T) {
	bin, args := LlamaArgs(LlamaArgsConfig{Binary: "/bin/llama-server", ModelPath: "/models/model.gguf", CtxSize: 4096, GPULayers: 10, NParallel: 2, Port: 8081, CacheTypeK: "q8_0", CacheTypeV: "q8_0"})
	if bin != "/bin/llama-server" {
		t.Errorf("unexpected binary: %s", bin)
	}
	found := map[string]bool{}
	for i := 0; i < len(args)-1; i++ {
		found[args[i]] = true
	}
	for _, flag := range []string{"--model", "--ctx-size", "--n-gpu-layers", "--parallel", "--port", "--host", "--cache-type-k", "--cache-type-v"} {
		if !found[flag] {
			t.Errorf("missing flag %s in args: %v", flag, args)
		}
	}
	if hasVerbose(args) {
		t.Errorf("--verbose must not appear when verbose=false: %v", args)
	}
}

func TestLlamaArgs_Verbose(t *testing.T) {
	_, args := LlamaArgs(LlamaArgsConfig{Binary: "/bin/llama-server", ModelPath: "/models/model.gguf", CtxSize: 4096, GPULayers: 10, NParallel: 2, Port: 8081, Verbose: true, CacheTypeK: "q8_0", CacheTypeV: "q8_0"})
	if !hasVerbose(args) {
		t.Errorf("expected --verbose when verbose=true, got %v", args)
	}
}

func TestLlamaArgs_CacheTypePassThrough(t *testing.T) {
	_, args := LlamaArgs(LlamaArgsConfig{Binary: "/bin/llama-server", ModelPath: "/models/model.gguf", CtxSize: 4096, GPULayers: 10, NParallel: 2, Port: 8081, CacheTypeK: "q4_0", CacheTypeV: "f16"})
	want := map[string]string{
		"--cache-type-k": "q4_0",
		"--cache-type-v": "f16",
	}
	for flag, val := range want {
		if got := flagValue(args, flag); got != val {
			t.Errorf("%s: got %q, want %q (args=%v)", flag, got, val, args)
		}
	}
}

func flagValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestEmbedderArgs(t *testing.T) {
	bin, args := EmbedderArgs(EmbedderArgsConfig{Binary: "/bin/embedder", ModelPath: "/models/embed.gguf", Port: 8082})
	if bin != "/bin/embedder" {
		t.Errorf("unexpected binary: %s", bin)
	}
	found := map[string]bool{}
	for _, a := range args {
		found[a] = true
	}
	for _, flag := range []string{"--model", "--embedding", "--n-gpu-layers", "--port", "--host"} {
		if !found[flag] {
			t.Errorf("missing flag %s in args: %v", flag, args)
		}
	}
	// --n-gpu-layers must be 0 so the embedder stays on CPU+RAM.
	for i, a := range args {
		if a == "--n-gpu-layers" {
			if i+1 >= len(args) || args[i+1] != "0" {
				t.Errorf("expected --n-gpu-layers 0, got args: %v", args)
			}
		}
	}
	if hasVerbose(args) {
		t.Errorf("--verbose must not appear when verbose=false: %v", args)
	}
}

func TestEmbedderArgs_Verbose(t *testing.T) {
	_, args := EmbedderArgs(EmbedderArgsConfig{Binary: "/bin/embedder", ModelPath: "/models/embed.gguf", Port: 8082, Verbose: true})
	if !hasVerbose(args) {
		t.Errorf("expected --verbose when verbose=true, got %v", args)
	}
}

func hasVerbose(args []string) bool {
	for _, a := range args {
		if a == "--verbose" {
			return true
		}
	}
	return false
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

func TestFormatExitLine(t *testing.T) {
	zero := 0
	ok := 1
	accessViolation := -1073741819 // 0xC0000005 on Windows
	tests := []struct {
		name string
		proc string
		code *int
		want string
	}{
		{
			name: "nil exit code",
			proc: "llama-server",
			code: nil,
			want: "[harness] llama-server exited (exit code unavailable)\n",
		},
		{
			name: "clean exit",
			proc: "llama-server",
			code: &zero,
			want: "[harness] llama-server exited (code 0 / 0x00000000)\n",
		},
		{
			name: "nonzero exit",
			proc: "embedder",
			code: &ok,
			want: "[harness] embedder exited (code 1 / 0x00000001)\n",
		},
		{
			name: "windows access violation",
			proc: "llama-server",
			code: &accessViolation,
			want: "[harness] llama-server exited (code -1073741819 / 0xC0000005)\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatExitLine(tc.proc, tc.code)
			if got != tc.want {
				t.Errorf("formatExitLine:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestEventKindConstants(t *testing.T) {
	kinds := []EventKind{EventStart, EventStop, EventHealthOK, EventHealthFail, EventRestart, EventError, EventFailed}
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

func TestRecordFailure_TripsAtThresholdWithinWindow(t *testing.T) {
	m := NewManager(ManagerConfig{
		Name:             "test",
		BuildArgs:        func() (string, []string) { return "binary", nil },
		HealthURL:        "http://127.0.0.1:9999/health",
		FailureThreshold: 3,
		FailureWindow:    time.Minute,
	})

	for i := 0; i < 2; i++ {
		if tripped := m.recordFailure(); tripped {
			t.Fatalf("tripped too early on failure %d", i+1)
		}
		if m.isFailed() {
			t.Fatalf("Failed=true too early on failure %d", i+1)
		}
	}

	if tripped := m.recordFailure(); !tripped {
		t.Fatal("expected recordFailure to trip on threshold")
	}
	if !m.isFailed() {
		t.Fatal("expected Failed=true after tripping")
	}
	if !m.Status().Failed {
		t.Fatal("Status.Failed should mirror isFailed")
	}

	// A subsequent call should not report tripped again - the UI emits once.
	if tripped := m.recordFailure(); tripped {
		t.Error("expected second trip to be false (already open)")
	}
}

func TestRecordFailure_EvictsOutsideWindow(t *testing.T) {
	m := NewManager(ManagerConfig{
		Name:             "test",
		BuildArgs:        func() (string, []string) { return "binary", nil },
		HealthURL:        "http://127.0.0.1:9999/health",
		FailureThreshold: 3,
		FailureWindow:    50 * time.Millisecond,
	})

	// Two stale failures that should roll out of the window.
	m.mu.Lock()
	stale := time.Now().Add(-time.Second)
	m.failures = []time.Time{stale, stale}
	m.mu.Unlock()

	if tripped := m.recordFailure(); tripped {
		t.Fatal("expected stale failures to be evicted so threshold is not met")
	}
	if m.isFailed() {
		t.Fatal("Failed should remain false after stale eviction")
	}
	m.mu.Lock()
	got := len(m.failures)
	m.mu.Unlock()
	if got != 1 {
		t.Fatalf("expected 1 live failure after eviction, got %d", got)
	}
}

func TestClearCircuit_ResetsFailedAndFailures(t *testing.T) {
	m := NewManager(ManagerConfig{
		Name:             "test",
		BuildArgs:        func() (string, []string) { return "binary", nil },
		HealthURL:        "http://127.0.0.1:9999/health",
		FailureThreshold: 1,
		FailureWindow:    time.Minute,
	})
	if !m.recordFailure() {
		t.Fatal("expected threshold 1 to trip on first failure")
	}
	m.clearCircuit()
	if m.isFailed() {
		t.Error("Failed should be false after clearCircuit")
	}
	m.mu.Lock()
	got := len(m.failures)
	m.mu.Unlock()
	if got != 0 {
		t.Errorf("failures should be empty after clearCircuit, got %d", got)
	}
}

func TestRestart_ClearsCircuitAndSignalsReload(t *testing.T) {
	m := NewManager(ManagerConfig{
		Name:             "test",
		BuildArgs:        func() (string, []string) { return "binary", nil },
		HealthURL:        "http://127.0.0.1:9999/health",
		FailureThreshold: 1,
		FailureWindow:    time.Minute,
	})
	_ = m.recordFailure()
	if !m.isFailed() {
		t.Fatal("precondition: expected Failed=true before Restart")
	}

	m.Restart()

	if m.isFailed() {
		t.Error("Restart should clear Failed")
	}
	select {
	case <-m.reloadCh:
	default:
		t.Error("Restart should signal reloadCh so Run wakes up")
	}
}

func TestMarkHealthy_ClearsLastError(t *testing.T) {
	// On a llama-server Restart the old child is killed, which makes the
	// next health tick fail with "connection refused" before the new child
	// has bound the port. Once the new child is up the banner must clear -
	// previously the stale error stuck around even though the badge had
	// flipped back to Healthy.
	m := NewManager(ManagerConfig{
		Name:      "test",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: "http://127.0.0.1:9999/health",
	})

	m.mu.Lock()
	m.healthy = false
	m.lastError = errors.New("dial tcp 127.0.0.1:8081: connectex: No connection could be made")
	m.mu.Unlock()

	m.markHealthy()

	s := m.Status()
	if !s.Healthy {
		t.Error("expected Healthy=true after markHealthy")
	}
	if s.LastError != nil {
		t.Errorf("expected LastError cleared, got %v", s.LastError)
	}
}

func TestCheckHealth_Returns503AsLoading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m := NewManager(ManagerConfig{
		Name:      "test",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: srv.URL,
	})

	err := m.checkHealth(context.Background())
	if !errors.Is(err, errLoading) {
		t.Fatalf("expected errLoading on 503, got %v", err)
	}
}

func TestCheckHealth_OtherStatusIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := NewManager(ManagerConfig{
		Name:      "test",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: srv.URL,
	})

	err := m.checkHealth(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, errLoading) {
		t.Fatalf("500 must not be classified as loading: %v", err)
	}
}

func TestCheckHealth_200IsHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewManager(ManagerConfig{
		Name:      "test",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: srv.URL,
	})

	if err := m.checkHealth(context.Background()); err != nil {
		t.Fatalf("expected nil on 200, got %v", err)
	}
}

func TestEmitExitLine_WritesToOutput(t *testing.T) {
	var buf bytes.Buffer
	m := NewManager(ManagerConfig{
		Name:      "llama-server",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: "http://127.0.0.1:9999/health",
		Output:    &buf,
	})
	code := -1073741819
	m.mu.Lock()
	m.exitCode = &code
	m.mu.Unlock()

	m.emitExitLine()

	want := "[harness] llama-server exited (code -1073741819 / 0xC0000005)\n"
	if got := buf.String(); got != want {
		t.Errorf("emitExitLine wrote %q, want %q", got, want)
	}
}

func TestEmitExitLine_NilOutputIsSafe(t *testing.T) {
	m := NewManager(ManagerConfig{
		Name:      "llama-server",
		BuildArgs: func() (string, []string) { return "binary", nil },
		HealthURL: "http://127.0.0.1:9999/health",
	})
	// m.output defaults to io.Discard, but cover the path explicitly.
	m.mu.Lock()
	m.output = nil
	m.mu.Unlock()
	// Must not panic.
	m.emitExitLine()
}

func TestRun_BlocksInFailedStateUntilReload(t *testing.T) {
	// A Run loop that enters the Failed state must stop calling BuildArgs
	// until Restart/Reconfigure wakes it. Pre-trip the breaker so the test
	// does not depend on how fast the OS rejects a bogus binary.
	var starts int32
	m := NewManager(ManagerConfig{
		Name: "test",
		BuildArgs: func() (string, []string) {
			atomic.AddInt32(&starts, 1)
			return "definitely-not-a-real-binary-name", nil
		},
		HealthURL:        "http://127.0.0.1:1/health",
		CheckPeriod:      10 * time.Millisecond,
		FailureThreshold: 2,
		FailureWindow:    time.Second,
	})

	m.recordFailure()
	m.recordFailure()
	if !m.isFailed() {
		t.Fatal("precondition: expected circuit tripped after two failures")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&starts); got != 0 {
		cancel()
		<-done
		t.Fatalf("Run called BuildArgs while Failed: got %d calls", got)
	}

	// Restart should wake the loop so it attempts to spawn again.
	m.Restart()
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&starts) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&starts) == 0 {
		cancel()
		<-done
		t.Fatal("Restart did not wake the Run loop")
	}

	cancel()
	<-done
}
