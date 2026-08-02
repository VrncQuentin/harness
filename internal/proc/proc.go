// Package proc manages child processes (llama-server, embedder sidecar).
// It spawns processes, runs health check loops, and restarts on failure
// with exponential backoff.
package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VrncQuentin/harness/internal/httpclient"
)

// errLoading is returned by checkHealth when the child reports that the
// model is still loading (HTTP 503 from llama-server / embedder during the
// load phase). The caller treats it as "keep polling, do not restart" -
// loading a multi-GB model can take longer than several check periods, so
// counting it as a failure tripped the breaker before the model was ever
// ready.
var errLoading = errors.New("proc: still loading")

// EventKind classifies a process manager event.
type EventKind string

const (
	EventStart      EventKind = "start"
	EventStop       EventKind = "stop"
	EventHealthOK   EventKind = "health_ok"
	EventHealthFail EventKind = "health_fail"
	EventRestart    EventKind = "restart"
	EventError      EventKind = "error"
	// EventFailed is emitted once when the circuit breaker trips. The Run
	// loop then blocks until Restart or Reconfigure is called.
	EventFailed EventKind = "failed"
)

// Defaults for the circuit breaker: a process that dies five times in a
// sixty-second window stops retrying and waits for the user to intervene.
const (
	defaultFailureThreshold = 5
	defaultFailureWindow    = 60 * time.Second
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
	// Failed is true when the circuit breaker has tripped: the process has
	// died too many times in the failure window and the Run loop is blocked
	// awaiting a Restart or Reconfigure.
	Failed bool
}

// Manager manages a single child process with health checking and restart logic.
type Manager struct {
	name        string
	events      chan<- Event
	checkPeriod time.Duration
	httpClient  *http.Client

	// reloadCh is signalled by Reconfigure and Restart. The Run loop treats
	// either as an intentional restart: backoff resets, the restart count is
	// not bumped, and the failure counted against the circuit breaker is
	// cleared.
	reloadCh chan struct{}

	// Circuit breaker knobs: if the process fails failureThreshold times
	// within failureWindow, the Run loop enters the Failed state and stops
	// retrying until the user clicks Restart or saves a new config.
	failureThreshold int
	failureWindow    time.Duration

	mu           sync.Mutex
	buildArgs    func() (string, []string)
	healthURL    string
	cmd          *exec.Cmd
	running      bool
	healthy      bool
	restartCount int
	lastError    error
	exitCode     *int
	output       io.Writer
	// failures holds the timestamps of recent unplanned exits, evicted as
	// they age past failureWindow. Cap on length is failureThreshold so the
	// slice stays tiny.
	failures []time.Time
	failed   bool
	done     chan struct{}
	// started records that Run was entered, so a shutdown can tell a manager
	// that is actually running (and whose done channel will close) from one
	// that was constructed but never launched.
	started atomic.Bool
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
	// Output receives the child's merged stdout+stderr. Nil discards.
	Output io.Writer
	// FailureThreshold is the number of unplanned exits within FailureWindow
	// that puts the manager into the Failed state. Zero picks the default.
	FailureThreshold int
	// FailureWindow is the rolling window used by the circuit breaker. Zero
	// picks the default.
	FailureWindow time.Duration
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
	out := cfg.Output
	if out == nil {
		out = io.Discard
	}
	threshold := cfg.FailureThreshold
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}
	window := cfg.FailureWindow
	if window <= 0 {
		window = defaultFailureWindow
	}
	return &Manager{
		name:             cfg.Name,
		buildArgs:        cfg.BuildArgs,
		healthURL:        cfg.HealthURL,
		events:           cfg.Events,
		checkPeriod:      period,
		httpClient:       hc,
		output:           out,
		reloadCh:         make(chan struct{}, 1),
		failureThreshold: threshold,
		failureWindow:    window,
		done:             make(chan struct{}),
	}
}

// Wait blocks until the manager Run loop has exited or ctx is cancelled.
func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Started reports whether the manager's Run loop was entered. A manager that
// was constructed but never launched never closes done, so waiting on it would
// only ever time out; Started lets shutdown paths skip that wait.
func (m *Manager) Started() bool { return m.started.Load() }

// Args returns the currently configured binary, arguments, and health URL.
// After Reconfigure these reflect the new configuration; the Run loop restarts
// the child asynchronously. The runtime uses it to compare what a process is
// actually configured to run against the recorded applied state.
func (m *Manager) Args() (string, []string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	build := m.buildArgs
	if build == nil {
		return "", nil, m.healthURL
	}
	binary, args := build()
	return binary, args, m.healthURL
}

// Reconfigure atomically swaps the args builder and health URL, then kills the
// running child so Run spins it up again under the new config. The restart is
// user-initiated, so it does not count against RestartCount, skips backoff,
// and clears any tripped circuit breaker.
func (m *Manager) Reconfigure(buildArgs func() (string, []string), healthURL string) {
	m.mu.Lock()
	m.buildArgs = buildArgs
	m.healthURL = healthURL
	m.mu.Unlock()

	m.Restart()
}

// Restart clears the circuit breaker and kicks the Run loop to try again with
// the currently-configured args. It is the manual escape hatch from the
// Failed state: the user sees the process is stuck, fixes whatever external
// issue caused the flap (freed a port, restarted a driver, etc.), and clicks
// Restart without needing to re-save config.
//
// RestartCount is not cleared - it reflects lifetime restarts across the
// Run loop, which the UI shows so the user can tell this process has been
// trouble.
func (m *Manager) Restart() {
	m.clearCircuit()
	select {
	case m.reloadCh <- struct{}{}:
	default:
	}
	m.stopProcess()
}

// recordFailure timestamps the most recent unplanned exit and evicts
// entries older than failureWindow. Returns true if this call tripped the
// breaker so the caller can emit EventFailed exactly once per trip.
func (m *Manager) recordFailure() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-m.failureWindow)
	kept := m.failures[:0]
	for _, t := range m.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	m.failures = append(kept, now)
	if !m.failed && len(m.failures) >= m.failureThreshold {
		m.failed = true
		return true
	}
	return false
}

// clearCircuit resets the circuit breaker. Called on user-initiated
// restarts (Restart, Reconfigure) and when Run exits the Failed-state wait.
func (m *Manager) clearCircuit() {
	m.mu.Lock()
	m.failed = false
	m.failures = nil
	m.mu.Unlock()
}

// isFailed reports whether the circuit breaker is currently tripped.
func (m *Manager) isFailed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failed
}

// Run is the main loop: start the process, health-check it, restart on failure.
// It blocks until ctx is cancelled. Callers are responsible for launching it
// as a goroutine: go mgr.Run(ctx).
func (m *Manager) Run(ctx context.Context) {
	m.started.Store(true)
	defer close(m.done)
	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		// Circuit-breaker gate: after too many failures in the window, park
		// here until Restart or Reconfigure signals reloadCh. No retries, no
		// sleeps, no churn - the UI shows Failed and the user drives.
		if m.isFailed() {
			select {
			case <-ctx.Done():
				return
			case <-m.reloadCh:
				m.clearCircuit()
				backoff = time.Second
				continue
			}
		}

		if err := m.startProcess(ctx); err != nil {
			m.setError(err)
			m.emit(EventError, fmt.Sprintf("failed to start: %v", err))
			if m.recordFailure() {
				m.emitCircuitOpen()
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-m.reloadCh:
				m.clearCircuit()
				backoff = time.Second
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
			continue
		}

		m.emit(EventStart, "process started")

		// Health check loop - runs until the process dies or context is cancelled.
		wasHealthy := m.healthLoop(ctx)
		if wasHealthy {
			backoff = time.Second
		}

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
			m.clearCircuit()
			backoff = time.Second
			m.emit(EventRestart, "reloading with new config")
			continue
		}

		if m.recordFailure() {
			m.emitCircuitOpen()
			continue
		}

		m.emit(EventRestart, fmt.Sprintf("restarting (attempt %d), backoff %s", m.restartCount, backoff))

		select {
		case <-ctx.Done():
			return
		case <-m.reloadCh:
			m.clearCircuit()
			backoff = time.Second
		case <-time.After(backoff):
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// emitCircuitOpen publishes a single EventFailed so the UI can surface the
// breaker trip in the log panel alongside the Failed badge.
func (m *Manager) emitCircuitOpen() {
	m.emit(EventFailed, fmt.Sprintf(
		"circuit open: %d failures within %s; waiting for restart or new config",
		m.failureThreshold, m.failureWindow,
	))
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
		Failed:       m.failed,
	}
}

// healthLoop polls the health endpoint every checkPeriod. Returns when the
// process exits (cmd.Wait returns) or ctx is cancelled.
func (m *Manager) healthLoop(ctx context.Context) bool {
	wasHealthy := false
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
			return wasHealthy
		case <-done:
			m.mu.Lock()
			m.running = false
			m.healthy = false
			m.mu.Unlock()
			m.emitExitLine()
			m.emit(EventStop, "process exited")
			return wasHealthy
		case <-ticker.C:
			err := m.checkHealth(ctx)
			switch {
			case errors.Is(err, errLoading):
				// Model is still loading (503). Keep polling; do not
				// kill or count as a failure - the load can take
				// minutes for multi-GB weights and we'd otherwise trip
				// the breaker before the model was ever ready.
				continue
			case err != nil:
				m.mu.Lock()
				m.healthy = false
				m.lastError = err
				m.mu.Unlock()
				m.emit(EventHealthFail, fmt.Sprintf("health check failed: %v", err))
				// Kill the process so the outer loop restarts it.
				m.stopProcess()
				// Wait briefly for cmd.Wait to record the exit code
				// before emitting the exit line, so the user sees the
				// same artefact as on a natural exit. Bounded so a
				// wedged kernel cannot hang the loop.
				select {
				case <-done:
				case <-time.After(2 * time.Second):
				}
				m.emitExitLine()
				return wasHealthy
			}
			wasHealthy = true
			m.markHealthy()
			m.emit(EventHealthOK, "healthy")
		}
	}
}

// emitExitLine writes the synthetic "[harness] <name> exited (code ...)"
// line to the proc output ring and also slogs it (newline trimmed - slog
// adds its own). Used by both the natural-exit and health-fail paths so
// the exit code is visible regardless of how termination was triggered.
func (m *Manager) emitExitLine() {
	m.mu.Lock()
	code := m.exitCode
	out := m.output
	m.mu.Unlock()
	line := formatExitLine(m.name, code)
	if out != nil {
		_, _ = io.WriteString(out, line)
	}
	slog.Info(strings.TrimSuffix(line, "\n"))
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
	// Merge stdout and stderr into one sink: llama-server and the embedder
	// split diagnostics across both streams, and Go would otherwise send
	// stdout to the null device, swallowing whatever landed there. Each
	// line arrives timestamped on the ring so the UI can show recent output
	// across restarts.
	cmd.Stdout = m.output
	cmd.Stderr = m.output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("proc: start %s: %w", m.name, err)
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

	// llama-server / embedder return 503 specifically while the model is
	// loading. Surface it as a typed sentinel so the caller can keep
	// polling instead of treating it as a failure.
	if resp.StatusCode == http.StatusServiceUnavailable {
		return errLoading
	}
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

// markHealthy flips the manager into the healthy state and clears any prior
// error. Without the clear, a transient failure (e.g. the brief connection
// refusal between killing the old llama-server and the new one binding the
// port on Restart) would leave its banner up forever even after the process
// is back to serving traffic.
func (m *Manager) markHealthy() {
	m.mu.Lock()
	m.healthy = true
	m.lastError = nil
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

// formatExitLine produces the synthetic log line appended to the process
// output ring when a child exits. The hex form matters on Windows because
// the common crash codes (0xC0000005 access violation, 0xC000013A Ctrl+C,
// 0xC00000FD stack overflow) are only recognisable in hex - the signed
// decimal form Go returns is opaque.
func formatExitLine(name string, code *int) string {
	if code == nil {
		return fmt.Sprintf("[harness] %s exited (exit code unavailable)\n", name)
	}
	return fmt.Sprintf("[harness] %s exited (code %d / 0x%08X)\n", name, *code, uint32(*code))
}
