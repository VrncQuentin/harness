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

	"github.com/vrnc/harness/pkg/httpclient"
)

// EventKind classifies a process manager event.
type EventKind string

const (
	EventStart      EventKind = "start"
	EventStop       EventKind = "stop"
	EventHealthOK   EventKind = "health_ok"
	EventHealthFail EventKind = "health_fail"
	EventRestart    EventKind = "restart"
	EventError      EventKind = "error"
)

// Event is a structured log event emitted by the process manager.
type Event struct {
	Time    time.Time
	Process string
	Kind    EventKind
	Message string
}

// Status describes the current state of a managed process.
type Status struct {
	Running      bool
	Healthy      bool
	RestartCount int
	LastError    error
	// ExitCode is non-nil once the child has exited at least once. It is
	// cleared on the next successful start so it only reports the most
	// recent exit while the process is back up.
	ExitCode *int
	// StderrTail is a recent, bounded window of the child's stderr. Empty
	// while the child is healthy; populated when a crash-looping child
	// needs to explain itself.
	StderrTail []string
}

// Manager manages a single child process with health checking and restart logic.
type Manager struct {
	name        string
	events      chan<- Event
	checkPeriod time.Duration
	httpClient  *http.Client

	// reloadCh is signalled by Reconfigure. The Run loop treats a reload as
	// an intentional restart: backoff resets and the restart count is not
	// bumped.
	reloadCh chan struct{}

	mu           sync.Mutex
	buildArgs    func() (string, []string)
	healthURL    string
	cmd          *exec.Cmd
	running      bool
	healthy      bool
	restartCount int
	lastError    error
	exitCode     *int
	stderr       *stderrBuffer
}

// ManagerConfig holds the configuration for a Manager.
type ManagerConfig struct {
	Name        string
	BuildArgs   func() (string, []string)
	HealthURL   string
	Events      chan<- Event
	CheckPeriod time.Duration
	// HTTPClient is used for health checks. Defaults to httpclient.New() if nil.
	HTTPClient *http.Client
}

// NewManager creates a new process Manager.
func NewManager(cfg ManagerConfig) *Manager {
	period := cfg.CheckPeriod
	if period == 0 {
		period = 5 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = httpclient.New()
	}
	return &Manager{
		name:        cfg.Name,
		buildArgs:   cfg.BuildArgs,
		healthURL:   cfg.HealthURL,
		events:      cfg.Events,
		checkPeriod: period,
		httpClient:  hc,
		stderr:      newStderrBuffer(64),
		reloadCh:    make(chan struct{}, 1),
	}
}

// Reconfigure atomically swaps the args builder and health URL, then kills the
// running child so Run spins it up again under the new config. The restart is
// user-initiated, so it does not count against RestartCount and skips backoff.
func (m *Manager) Reconfigure(buildArgs func() (string, []string), healthURL string) {
	m.mu.Lock()
	m.buildArgs = buildArgs
	m.healthURL = healthURL
	m.mu.Unlock()

	select {
	case m.reloadCh <- struct{}{}:
	default:
	}
	m.stopProcess()
}

// Run is the main loop: start the process, health-check it, restart on failure.
// It blocks until ctx is cancelled. Callers are responsible for launching it
// as a goroutine: go mgr.Run(ctx).
func (m *Manager) Run(ctx context.Context) {
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		if err := m.startProcess(ctx); err != nil {
			m.setError(err)
			m.emit(EventError, fmt.Sprintf("failed to start: %v", err))
			select {
			case <-ctx.Done():
				return
			case <-m.reloadCh:
				backoff = time.Second
				continue
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}

		backoff = time.Second // reset on successful start
		m.emit(EventStart, "process started")

		// Health check loop - runs until the process dies or context is cancelled.
		m.healthLoop(ctx)

		if ctx.Err() != nil {
			m.stopProcess()
			return
		}

		// Drain a pending reload signal: if the exit was user-initiated we
		// skip the failure accounting and restart immediately with fresh args.
		reloaded := false
		select {
		case <-m.reloadCh:
			reloaded = true
		default:
		}

		m.mu.Lock()
		if !reloaded {
			m.restartCount++
		}
		m.running = false
		m.healthy = false
		m.mu.Unlock()

		if reloaded {
			backoff = time.Second
			m.emit(EventRestart, "reloading with new config")
			continue
		}

		m.emit(EventRestart, fmt.Sprintf("restarting (attempt %d), backoff %s", m.restartCount, backoff))

		select {
		case <-ctx.Done():
			return
		case <-m.reloadCh:
			backoff = time.Second
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// Status returns the current status of the managed process.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ec *int
	if m.exitCode != nil {
		v := *m.exitCode
		ec = &v
	}
	return Status{
		Running:      m.running,
		Healthy:      m.healthy,
		RestartCount: m.restartCount,
		LastError:    m.lastError,
		ExitCode:     ec,
		StderrTail:   m.stderr.Snapshot(),
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
			if cmd.ProcessState != nil {
				code := cmd.ProcessState.ExitCode()
				m.mu.Lock()
				m.exitCode = &code
				m.mu.Unlock()
			}
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
			m.emit(EventStop, "process exited")
			return
		case <-ticker.C:
			if err := m.checkHealth(ctx); err != nil {
				m.mu.Lock()
				m.healthy = false
				m.lastError = err
				m.mu.Unlock()
				m.emit(EventHealthFail, fmt.Sprintf("health check failed: %v", err))
				// Kill the process so the outer loop restarts it.
				m.stopProcess()
				return
			}
			m.mu.Lock()
			m.healthy = true
			m.mu.Unlock()
			m.emit(EventHealthOK, "healthy")
		}
	}
}

// startProcess spawns the child process. Uses CommandContext so the OS kills
// the child automatically when ctx is cancelled.
func (m *Manager) startProcess(ctx context.Context) error {
	m.mu.Lock()
	build := m.buildArgs
	m.mu.Unlock()
	binary, args := build()
	cmd := exec.CommandContext(ctx, binary, args...)
	hideConsole(cmd)
	m.stderr.Reset()
	cmd.Stderr = m.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: failed to start %s: %w", m.name, err)
	}
	m.mu.Lock()
	m.cmd = cmd
	m.running = true
	m.healthy = false
	m.exitCode = nil
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

	m.mu.Lock()
	url := m.healthURL
	m.mu.Unlock()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("proc: build health request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("proc: health GET %s: %w", url, err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= 300 {
		return fmt.Errorf("proc: health check returned status %d", resp.StatusCode)
	}
	return nil
}

// setError stores the last error under the lock.
func (m *Manager) setError(err error) {
	m.mu.Lock()
	m.lastError = err
	m.mu.Unlock()
}

// emit sends an event on the events channel (non-blocking).
func (m *Manager) emit(kind EventKind, msg string) {
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

// LlamaArgs builds the argument slice for llama-server.
func LlamaArgs(binary, modelPath string, ctxSize, gpuLayers, nParallel, port int) (string, []string) {
	return binary, []string{
		"--model", modelPath,
		"--ctx-size", fmt.Sprintf("%d", ctxSize),
		"--n-gpu-layers", fmt.Sprintf("%d", gpuLayers),
		"--parallel", fmt.Sprintf("%d", nParallel),
		"--port", fmt.Sprintf("%d", port),
		"--host", "127.0.0.1",
	}
}

// EmbedderArgs builds the argument slice for the embedder sidecar.
// --embedding switches llama-server into embedding mode; without it the
// server boots a chat-completion endpoint and /embedding returns 501,
// which defeats the whole point of running a second process.
func EmbedderArgs(binary, modelPath string, port int) (string, []string) {
	return binary, []string{
		"--model", modelPath,
		"--embedding",
		"--port", fmt.Sprintf("%d", port),
		"--host", "127.0.0.1",
	}
}
