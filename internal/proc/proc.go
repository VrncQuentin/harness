// Package proc manages child processes (llama-server, embedder sidecar).
// It spawns processes, runs health check loops, and restarts on failure
// with exponential backoff.
package proc

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// Event is a structured log event emitted by the process manager.
type Event struct {
	Time    time.Time
	Process string
	Kind    string // "start", "stop", "health_ok", "health_fail", "restart", "error"
	Message string
}

// Status describes the current state of a managed process.
type Status struct {
	Running      bool
	Healthy      bool
	RestartCount int
	LastError    string
}

// Manager manages a single child process with health checking and restart logic.
type Manager struct {
	name        string
	buildArgs   func() (string, []string)
	healthURL   string
	events      chan<- Event
	checkPeriod time.Duration

	mu           sync.Mutex
	cmd          *exec.Cmd
	running      bool
	healthy      bool
	restartCount int
	lastError    string
}

// ManagerConfig holds the configuration for a Manager.
type ManagerConfig struct {
	Name        string
	BuildArgs   func() (string, []string)
	HealthURL   string
	Events      chan<- Event
	CheckPeriod time.Duration
}

// NewManager creates a new process Manager.
func NewManager(cfg ManagerConfig) *Manager {
	period := cfg.CheckPeriod
	if period == 0 {
		period = 5 * time.Second
	}
	return &Manager{
		name:        cfg.Name,
		buildArgs:   cfg.BuildArgs,
		healthURL:   cfg.HealthURL,
		events:      cfg.Events,
		checkPeriod: period,
	}
}

// Start begins the process and the health check loop. It returns immediately;
// the process runs in background goroutines. ctx cancellation triggers shutdown.
func (m *Manager) Start(ctx context.Context) {
	go m.run(ctx)
}

// Status returns the current status of the managed process.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Running:      m.running,
		Healthy:      m.healthy,
		RestartCount: m.restartCount,
		LastError:    m.lastError,
	}
}

// run is the main loop: start the process, health-check it, restart on failure.
func (m *Manager) run(ctx context.Context) {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		if err := m.startProcess(); err != nil {
			m.setError(err.Error())
			m.emit("error", fmt.Sprintf("failed to start: %v", err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}

		backoff = time.Second // reset on successful start
		m.emit("start", "process started")

		// Health check loop — runs until the process dies or context is cancelled.
		m.healthLoop(ctx)

		if ctx.Err() != nil {
			m.stopProcess()
			return
		}

		// Process died or health loop exited — increment restart count and retry.
		m.mu.Lock()
		m.restartCount++
		m.running = false
		m.healthy = false
		m.mu.Unlock()

		m.emit("restart", fmt.Sprintf("restarting (attempt %d), backoff %s", m.restartCount, backoff))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// healthLoop polls the health endpoint every checkPeriod. Returns when the
// process exits (cmd.Wait returns) or ctx is cancelled.
func (m *Manager) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.checkPeriod)
	defer ticker.Stop()

	// Channel that fires when the process exits.
	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		cmd := m.cmd
		m.mu.Unlock()
		if cmd != nil {
			cmd.Wait() //nolint:errcheck
		}
		close(done)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			m.mu.Lock()
			m.running = false
			m.healthy = false
			m.mu.Unlock()
			m.emit("stop", "process exited")
			return
		case <-ticker.C:
			if err := m.checkHealth(ctx); err != nil {
				m.mu.Lock()
				m.healthy = false
				m.lastError = err.Error()
				m.mu.Unlock()
				m.emit("health_fail", fmt.Sprintf("health check failed: %v", err))
				// Kill the process so the outer loop restarts it.
				m.stopProcess()
				return
			}
			m.mu.Lock()
			m.healthy = true
			m.mu.Unlock()
			m.emit("health_ok", "healthy")
		}
	}
}

// startProcess spawns the child process.
func (m *Manager) startProcess() error {
	binary, args := m.buildArgs()
	cmd := exec.Command(binary, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: failed to start %s: %w", m.name, err)
	}
	m.mu.Lock()
	m.cmd = cmd
	m.running = true
	m.healthy = false
	m.mu.Unlock()
	return nil
}

// stopProcess kills the child process if it is running.
func (m *Manager) stopProcess() {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill() //nolint:errcheck
	}
}

// checkHealth performs a single HTTP GET against the health URL.
func (m *Manager) checkHealth(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, m.healthURL, nil)
	if err != nil {
		return fmt.Errorf("proc: build health request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("proc: health GET %s: %w", m.healthURL, err)
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("proc: health check returned status %d", resp.StatusCode)
	}
	return nil
}

// setError stores the last error message under the lock.
func (m *Manager) setError(msg string) {
	m.mu.Lock()
	m.lastError = msg
	m.mu.Unlock()
}

// emit sends an event on the events channel (non-blocking).
func (m *Manager) emit(kind, msg string) {
	if m.events == nil {
		return
	}
	select {
	case m.events <- Event{
		Time:    time.Now(),
		Process: m.name,
		Kind:    kind,
		Message: msg,
	}:
	default:
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// LlamaArgs builds the argument slice for llama-server.
func LlamaArgs(binary, modelPath string, ctxSize, gpuLayers, nParallel int) (string, []string) {
	return binary, []string{
		"--model", modelPath,
		"--ctx-size", fmt.Sprintf("%d", ctxSize),
		"--n-gpu-layers", fmt.Sprintf("%d", gpuLayers),
		"--parallel", fmt.Sprintf("%d", nParallel),
		"--port", "8081",
		"--host", "127.0.0.1",
	}
}

// EmbedderArgs builds the argument slice for the embedder sidecar.
func EmbedderArgs(binary, modelPath string) (string, []string) {
	return binary, []string{
		"--model", modelPath,
		"--port", "8082",
		"--host", "127.0.0.1",
	}
}
