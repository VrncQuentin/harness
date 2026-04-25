// Package ui implements the browser-based management UI server.
package ui

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrnc/harness/assets"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/logbuf"
)

// RetryFunc is called when the user clicks Retry on the status page or saves
// a new config. The implementation (in main) is responsible for clearing and
// re-adding startup errors via the Server's methods, and returns what the
// reload achieved vs. what still needs a full harness restart.
type RetryFunc func() ApplyResult

// ApplyResult reports the outcome of re-reading + re-applying the saved config.
// LiveApplied is true when at least one component was reconfigured in place.
// RestartNeeded lists human-readable reasons (e.g. "UI port") for changes that
// require a full harness restart to take effect.
type ApplyResult struct {
	LiveApplied   bool
	RestartNeeded []string
}

// ProcessStatus is the UI-facing status of a managed process.
type ProcessStatus struct {
	Name         string
	Running      bool
	Healthy      bool
	RestartCount int
	LastError    error
	ExitCode     *int
	// Failed is true when the circuit breaker has tripped. The status page
	// renders a Failed badge and a Restart button in place of the usual
	// Unhealthy state so the user knows no more auto-retries are coming.
	Failed bool
}

// stateSnapshot holds the copyable fields of State (no mutex).
type stateSnapshot struct {
	LlamaStatus    ProcessStatus
	EmbedderStatus ProcessStatus
	QueueDepth     int
	QueueMax       int
	StartupErrors  []error
	FirstRun       bool
	StartTime      time.Time
}

// State is the protected mutable state of the UI server.
type State struct {
	mu   sync.RWMutex
	data stateSnapshot
}

func (s *State) snapshot() stateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Server is the UI HTTP server.
type Server struct {
	port  int
	state *State

	// sseClients maps chan string → struct{} for active SSE subscribers.
	sseClients sync.Map

	// Each page has its own template set because status.html, config.html,
	// and agents.html all define "title" and "content" - sharing one set
	// would let the later parse clobber the earlier one.
	statusTmpl     *template.Template
	configTmpl     *template.Template
	agentsTmpl     *template.Template
	memoryTmpl     *template.Template
	memoryEditTmpl *template.Template

	retryMu sync.RWMutex
	retry   RetryFunc

	// procRestartMu guards the manual-restart callbacks wired by main
	// once the process managers exist. The Restart button on a proc card
	// POSTs to /procs/{name}/restart and we invoke the matching callback.
	procRestartMu sync.RWMutex
	llamaRestart  func()
	embedRestart  func()

	storeMu sync.RWMutex
	store   config.Store

	agentRegMu sync.RWMutex
	agentReg   AgentRegistry

	binDirMu sync.RWMutex
	binDir   string

	// memRepo is the configured memory repo path. Empty means the user
	// has not set one yet, in which case the status page suppresses the
	// layout-scaffolding prompt entirely.
	memRepoMu sync.RWMutex
	memRepo   string

	memStoreMu sync.RWMutex
	memStore   MemoryStore

	logRing   atomic.Pointer[logbuf.Ring]
	llamaRing atomic.Pointer[logbuf.Ring]
	embedRing atomic.Pointer[logbuf.Ring]
}

// NewServer creates a new UI server on the given port. The config store is
// injected separately via SetConfigStore once the shared database is open.
func NewServer(port int) *Server {
	s := &Server{
		port:  port,
		state: &State{data: stateSnapshot{StartTime: time.Now()}},
	}
	s.statusTmpl = template.Must(template.ParseFS(
		assets.TemplateFS,
		"templates/layout.html",
		"templates/status.html",
	))
	s.configTmpl = template.Must(template.ParseFS(
		assets.TemplateFS,
		"templates/layout.html",
		"templates/config.html",
	))
	s.agentsTmpl = template.Must(template.ParseFS(
		assets.TemplateFS,
		"templates/layout.html",
		"templates/agents.html",
	))
	s.memoryTmpl = template.Must(template.ParseFS(
		assets.TemplateFS,
		"templates/layout.html",
		"templates/memory.html",
	))
	s.memoryEditTmpl = template.Must(template.ParseFS(
		assets.TemplateFS,
		"templates/layout.html",
		"templates/memory_edit.html",
	))
	return s
}

// SetRetry installs the callback invoked on /retry and after a successful
// config save. Safe to call at any time; calls before it is set are no-ops.
func (s *Server) SetRetry(fn RetryFunc) {
	s.retryMu.Lock()
	s.retry = fn
	s.retryMu.Unlock()
}

func (s *Server) callRetry() ApplyResult {
	s.retryMu.RLock()
	fn := s.retry
	s.retryMu.RUnlock()
	if fn == nil {
		return ApplyResult{}
	}
	return fn()
}

func (s *Server) hasRetry() bool {
	s.retryMu.RLock()
	defer s.retryMu.RUnlock()
	return s.retry != nil
}

// SetProcRestarts installs the callbacks used by the /procs/{name}/restart
// endpoints. Either may be nil while the matching manager is not yet up; the
// handler treats nil as a no-op and still redirects the user back to /.
func (s *Server) SetProcRestarts(llama, embed func()) {
	s.procRestartMu.Lock()
	s.llamaRestart = llama
	s.embedRestart = embed
	s.procRestartMu.Unlock()
}

func (s *Server) getProcRestart(name string) func() {
	s.procRestartMu.RLock()
	defer s.procRestartMu.RUnlock()
	switch name {
	case "llama":
		return s.llamaRestart
	case "embed":
		return s.embedRestart
	default:
		return nil
	}
}

// SetConfigStore installs the config store used by the /config page. If nil,
// the config page renders an error instead of a form.
func (s *Server) SetConfigStore(store config.Store) {
	s.storeMu.Lock()
	s.store = store
	s.storeMu.Unlock()
}

func (s *Server) configStore() config.Store {
	s.storeMu.RLock()
	defer s.storeMu.RUnlock()
	return s.store
}

// SetBinDir records the directory containing the running harness binary. It is
// used by the /config page to suggest detected llama-server and .gguf paths.
// Safe to leave unset; detection then returns no suggestions.
func (s *Server) SetBinDir(dir string) {
	s.binDirMu.Lock()
	s.binDir = dir
	s.binDirMu.Unlock()
}

func (s *Server) getBinDir() string {
	s.binDirMu.RLock()
	defer s.binDirMu.RUnlock()
	return s.binDir
}

// SetMemoryRepoPath records the configured memory repo path. The status
// page uses it to detect a missing canonical layout (global/, agents/,
// etc.) and surface a prompt to scaffold what is missing. Pass "" to
// clear (e.g. when the user removes the path from /config).
func (s *Server) SetMemoryRepoPath(path string) {
	s.memRepoMu.Lock()
	s.memRepo = path
	s.memRepoMu.Unlock()
}

func (s *Server) getMemoryRepoPath() string {
	s.memRepoMu.RLock()
	defer s.memRepoMu.RUnlock()
	return s.memRepo
}

// SetLogRing wires the harness log ring into the UI so the status page can
// show recent output and stream new entries over SSE. Safe to leave unset;
// the log panel then renders empty and the SSE endpoint returns 503.
func (s *Server) SetLogRing(r *logbuf.Ring) {
	s.logRing.Store(r)
}

// SetLlamaOutputRing wires the ring that receives llama-server stdout+stderr.
// The status page's llama card subscribes over SSE to stream new lines.
func (s *Server) SetLlamaOutputRing(r *logbuf.Ring) {
	s.llamaRing.Store(r)
}

// SetEmbedOutputRing wires the ring that receives the embedder's stdout+stderr.
func (s *Server) SetEmbedOutputRing(r *logbuf.Ring) {
	s.embedRing.Store(r)
}

func (s *Server) getLogRing() *logbuf.Ring   { return s.logRing.Load() }
func (s *Server) getLlamaRing() *logbuf.Ring { return s.llamaRing.Load() }
func (s *Server) getEmbedRing() *logbuf.Ring { return s.embedRing.Load() }

// SetLlamaStatus updates the llama-server status.
func (s *Server) SetLlamaStatus(st ProcessStatus) {
	s.state.mu.Lock()
	s.state.data.LlamaStatus = st
	s.state.mu.Unlock()
	s.broadcastState()
}

// SetEmbedderStatus updates the embedder status.
func (s *Server) SetEmbedderStatus(st ProcessStatus) {
	s.state.mu.Lock()
	s.state.data.EmbedderStatus = st
	s.state.mu.Unlock()
	s.broadcastState()
}

// SetQueueDepth updates the queue depth and the configured maximum.
func (s *Server) SetQueueDepth(depth, capacity int) {
	s.state.mu.Lock()
	s.state.data.QueueDepth = depth
	s.state.data.QueueMax = capacity
	s.state.mu.Unlock()
	s.broadcastState()
}

// AddStartupError appends a startup error.
func (s *Server) AddStartupError(err error) {
	s.state.mu.Lock()
	s.state.data.StartupErrors = append(s.state.data.StartupErrors, err)
	s.state.mu.Unlock()
	s.broadcastState()
}

// ClearStartupErrors clears all startup errors.
func (s *Server) ClearStartupErrors() {
	s.state.mu.Lock()
	s.state.data.StartupErrors = nil
	s.state.mu.Unlock()
	s.broadcastState()
}

// SetFirstRun toggles the "user has never saved config" banner. When true,
// the status page shows a "Set up your harness" CTA instead of startup errors.
func (s *Server) SetFirstRun(v bool) {
	s.state.mu.Lock()
	s.state.data.FirstRun = v
	s.state.mu.Unlock()
	s.broadcastState()
}

// Start begins serving on the configured port in a background goroutine.
// It returns an error immediately if the listener cannot bind.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatus)
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/logs/harness", s.streamRing(s.getLogRing))
	mux.HandleFunc("/logs/llama", s.streamRing(s.getLlamaRing))
	mux.HandleFunc("/logs/embed", s.streamRing(s.getEmbedRing))
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/agents", s.handleAgents)
	mux.HandleFunc("/agents/active", s.handleAgentsActive)
	mux.HandleFunc("/agents/create", s.handleAgentsCreate)
	mux.HandleFunc("/memory", s.handleMemory)
	mux.HandleFunc("/memory/edit", s.handleMemoryEdit)
	mux.HandleFunc("/memory/save", s.handleMemorySave)
	mux.HandleFunc("/retry", s.handleRetry)
	mux.HandleFunc("/memory/scaffold", s.handleMemoryScaffold)
	mux.HandleFunc("/procs/llama/restart", s.handleProcRestart("llama"))
	mux.HandleFunc("/procs/embed/restart", s.handleProcRestart("embed"))
	mux.Handle("/static/", http.FileServer(http.FS(assets.StaticFS)))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	// Verify we can bind before returning.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("ui: bind :%d: %w", s.port, err)
	}

	go func() {
		srv.Serve(ln) //nolint:errcheck
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	return nil
}

// basePage holds template fields shared by every rendered page (nav highlight
// and footer uptime live in layout.html).
type basePage struct {
	Page       string
	UptimeText string
}

func (s *Server) newBasePage(page string) basePage {
	return basePage{
		Page:       page,
		UptimeText: formatUptime(time.Since(s.state.snapshot().StartTime)),
	}
}

func formatUptime(d time.Duration) string {
	s := int64(d.Seconds())
	if s < 0 {
		s = 0
	}
	days := s / 86400
	s -= days * 86400
	h := s / 3600
	s -= h * 3600
	m := s / 60
	s -= m * 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
