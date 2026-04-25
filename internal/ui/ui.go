// Package ui implements the browser-based management UI server.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrnc/harness/assets"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/logbuf"
)

// statusLogTail is the number of recent log entries shown server-rendered on
// the status page. New entries are appended client-side via SSE.
const statusLogTail = 100

// procLogTail is the number of recent output lines shown server-rendered for
// each proc's log card. New entries are appended client-side via SSE, so this
// only controls the initial seed on page load.
const procLogTail = 50

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

	// Each page has its own template set because status.html and config.html
	// both define "title" and "content" - sharing one set would let the later
	// parse clobber the earlier one.
	statusTmpl *template.Template
	configTmpl *template.Template

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

	binDirMu sync.RWMutex
	binDir   string

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
	mux.HandleFunc("/retry", s.handleRetry)
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

// statusPageData is the template context for the status page.
type statusPageData struct {
	basePage
	stateSnapshot
	QueuePct       int
	HasRetry       bool
	StartupErrText []string
	HarnessLog     logboxData
	LlamaLog       logboxData
	EmbedLog       logboxData
}

// logboxData is the data passed to the shared "logbox" template partial. One
// instance per card: harness logs, llama-server output, embedder output.
type logboxData struct {
	BodyID  string
	Entries []logEntryView
}

// logEntryView is the template-friendly form of a logbuf entry.
type logEntryView struct {
	Time string
	Line string
}

// configPageData is the template context for the config editor.
type configPageData struct {
	basePage
	Config         *config.Config
	Suggestions    config.Suggestions
	FirstRun       bool
	Saved          bool
	LiveApplied    bool
	RestartReasons []string
	ValidationErr  string
	SaveErr        string
}

// handleStatus renders the status page. Only the root path renders the page;
// any other unknown path returns 404 so we don't shadow future routes.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	snap := s.state.snapshot()

	errTexts := make([]string, 0, len(snap.StartupErrors))
	for _, e := range snap.StartupErrors {
		errTexts = append(errTexts, e.Error())
	}

	data := statusPageData{
		basePage:       s.newBasePage("status"),
		stateSnapshot:  snap,
		QueuePct:       queuePct(snap.QueueDepth, snap.QueueMax),
		HasRetry:       s.hasRetry(),
		StartupErrText: errTexts,
		HarnessLog:     logboxData{BodyID: "harness-log", Entries: recentEntries(s.getLogRing(), statusLogTail)},
		LlamaLog:       logboxData{BodyID: "llama-log", Entries: recentEntries(s.getLlamaRing(), procLogTail)},
		EmbedLog:       logboxData{BodyID: "embed-log", Entries: recentEntries(s.getEmbedRing(), procLogTail)},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.statusTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) hasRetry() bool {
	s.retryMu.RLock()
	defer s.retryMu.RUnlock()
	return s.retry != nil
}

func queuePct(depth, capacity int) int {
	if capacity <= 0 {
		return 0
	}
	p := depth * 100 / capacity
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
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

// recentEntries returns the last n log entries from ring formatted for the
// status template. Returns nil if the ring is not wired up.
func recentEntries(ring *logbuf.Ring, n int) []logEntryView {
	if ring == nil {
		return nil
	}
	all := ring.Snapshot()
	if len(all) > n {
		all = all[len(all)-n:]
	}
	out := make([]logEntryView, len(all))
	for i, e := range all {
		out[i] = logEntryView{
			Time: e.Time.Format("15:04:05"),
			Line: e.Line,
		}
	}
	return out
}

// streamRing returns an SSE handler that streams new entries from the ring
// returned by get. get is called per-request so callers can install the ring
// lazily via a setter and have streams pick it up on the next connection.
// The initial snapshot is rendered server-side in the status page; this
// stream only carries entries written after the connection was opened.
func (s *Server) streamRing(get func() *logbuf.Ring) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ring := get()
		if ring == nil {
			http.Error(w, "log ring not configured", http.StatusServiceUnavailable)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Subscribe before the first flush so no entry written between "headers
		// out" and "loop entered" is missed.
		//
		// Buffer 10k entries to absorb verbose-mode bursts: llama.cpp emits
		// dozens of lines per Write syscall during model load and SSE delivery
		// (JSON encode + Fprintf + Flush per line) is much slower than the
		// ring's append, so the channel has to bridge the rate mismatch. The
		// ring drops on overflow rather than blocking the writer; with 10k
		// slots a stalled subscriber loses lines instead of stalling
		// llama-server's stdout pipe. Memory cost is ~400 KB per subscriber.
		ch := make(chan logbuf.Entry, 10000)
		cancel := ring.Subscribe(ch)
		defer cancel()

		// Flush an SSE comment immediately so headers go out the door and the
		// browser fires onopen. Without this the connection sits header-less
		// until the first log line, which for a quiet harness can be many
		// minutes and leaves the panel looking stuck.
		if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
			return
		}
		flusher.Flush()

		// Heartbeat so idle connections stay warm and a dropped client is
		// noticed promptly (the Write fails and we exit).
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case e, ok := <-ch:
				if !ok {
					return
				}
				payload, err := json.Marshal(struct {
					Time string `json:"time"`
					Line string `json:"line"`
				}{
					Time: e.Time.Format("15:04:05"),
					Line: e.Line,
				})
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}

// handleSSE streams JSON state updates to the client via Server-Sent Events.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Per-client SSE buffer. Senders (broadcastState, sendState) use
	// non-blocking sends and drop on overflow, so 1 is enough: a missed
	// payload is replaced by the next one within the 2s ticker.
	ch := make(chan string, 1)
	s.sseClients.Store(ch, struct{}{})
	defer s.sseClients.Delete(ch)

	// Send initial state immediately.
	s.sendState(ch)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			s.sendState(ch)
		}
	}
}

// broadcastState sends the current state to all SSE clients.
func (s *Server) broadcastState() {
	b, _ := json.Marshal(stateToPayload(s.state.snapshot()))
	msg := string(b)

	s.sseClients.Range(func(key, _ any) bool {
		ch, ok := key.(chan string)
		if !ok {
			return true
		}
		select {
		case ch <- msg:
		default:
		}
		return true
	})
}

// sendState marshals the current state and sends it to a specific client channel.
func (s *Server) sendState(ch chan string) {
	b, _ := json.Marshal(stateToPayload(s.state.snapshot()))
	select {
	case ch <- string(b):
	default:
	}
}

type ssePayload struct {
	LlamaHealthy  bool     `json:"llama_healthy"`
	LlamaRunning  bool     `json:"llama_running"`
	LlamaRestarts int      `json:"llama_restarts"`
	LlamaFailed   bool     `json:"llama_failed"`
	EmbedHealthy  bool     `json:"embed_healthy"`
	EmbedRunning  bool     `json:"embed_running"`
	EmbedRestarts int      `json:"embed_restarts"`
	EmbedFailed   bool     `json:"embed_failed"`
	QueueDepth    int      `json:"queue_depth"`
	QueueMax      int      `json:"queue_max"`
	StartupErrors []string `json:"startup_errors,omitempty"`
	FirstRun      bool     `json:"first_run"`
	UptimeSeconds int64    `json:"uptime_seconds"`
}

// handleConfig serves GET (render form) and POST (save + re-validate).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderConfig(w, r, configPageData{}, false /* skipStoreLoad */)
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// renderConfig renders /config with the given overlay data (error/success flags).
// If cfg in overlay is nil, it is populated from the store.
func (s *Server) renderConfig(w http.ResponseWriter, r *http.Request, overlay configPageData, skipStoreLoad bool) {
	data := overlay
	data.basePage = s.newBasePage("config")

	if data.Config == nil && !skipStoreLoad {
		store := s.configStore()
		if store == nil {
			data.SaveErr = "config store unavailable (harness.db could not be opened)"
			d := config.Defaults()
			data.Config = &d
			data.FirstRun = true
		} else {
			cfg, configured, err := store.Load()
			if err != nil {
				data.SaveErr = err.Error()
				d := config.Defaults()
				data.Config = &d
				data.FirstRun = true
			} else {
				data.Config = cfg
				data.FirstRun = !configured
			}
		}
	}
	if r.URL.Query().Get("saved") == "1" {
		data.Saved = true
		data.LiveApplied = r.URL.Query().Get("applied") == "1"
		if rs := r.URL.Query().Get("restart"); rs != "" {
			data.RestartReasons = strings.Split(rs, "|")
		}
	}

	data.Suggestions = config.Detect(s.getBinDir())
	// On a fresh GET render, pre-fill model_binary with the first detected
	// llama-server if the user has not entered one yet. We do not pre-fill
	// anything else - datalists let the user pick without us guessing.
	if !skipStoreLoad && data.Config != nil && data.Config.Model.Binary == "" && len(data.Suggestions.LlamaBinary) > 0 {
		data.Config.Model.Binary = data.Suggestions.LlamaBinary[0]
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.configTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// saveConfig parses the form, validates, writes, then triggers retry.
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConfig(w, r, configPageData{SaveErr: "could not parse form: " + err.Error()}, true /* skipStoreLoad */)
		return
	}

	store := s.configStore()
	if store == nil {
		s.renderConfig(w, r, configPageData{SaveErr: "config store unavailable"}, true /* skipStoreLoad */)
		return
	}

	// Use the current saved config as the base so fields the form doesn't
	// touch (or numeric fields left blank) preserve their existing values
	// rather than snapping back to Defaults.
	base := config.Defaults()
	if cur, _, err := store.Load(); err == nil {
		base = *cur
	}
	cfg := parseConfigForm(r, &base)

	if err := config.Validate(cfg); err != nil {
		s.renderConfig(w, r, configPageData{Config: cfg, ValidationErr: err.Error()}, true /* skipStoreLoad */)
		return
	}
	if err := store.Save(cfg); err != nil {
		s.renderConfig(w, r, configPageData{Config: cfg, SaveErr: err.Error()}, true /* skipStoreLoad */)
		return
	}

	// Trigger retry so startup errors get cleared/refreshed against the new config.
	result := s.callRetry()
	target := "/config?saved=1"
	if result.LiveApplied {
		target += "&applied=1"
	}
	if len(result.RestartNeeded) > 0 {
		target += "&restart=" + url.QueryEscape(strings.Join(result.RestartNeeded, "|"))
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleRetry is POST /retry - clears startup errors and re-runs validation.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.callRetry()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleProcRestart is POST /procs/{name}/restart - invokes the manager's
// manual restart, clearing its circuit breaker. A missing callback is a
// no-op (the manager isn't up yet) and the user is redirected either way so
// the updated status flows through SSE without a blank page.
func (s *Server) handleProcRestart(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if fn := s.getProcRestart(name); fn != nil {
			fn()
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// parseConfigForm builds a Config from the posted form, overlaying values on
// base. Numeric fields that are missing or unparseable keep the base value;
// string fields are always overwritten (an empty required field will surface
// as a validation error downstream).
func parseConfigForm(r *http.Request, base *config.Config) *config.Config {
	cfg := *base

	cfg.Model.Binary = strings.TrimSpace(r.FormValue("model_binary"))
	cfg.Model.ModelPath = strings.TrimSpace(r.FormValue("model_path"))
	cfg.Model.CtxSize = atoiOr(r.FormValue("model_ctx_size"), cfg.Model.CtxSize)
	cfg.Model.GPULayers = atoiOr(r.FormValue("model_gpu_layers"), cfg.Model.GPULayers)
	cfg.Model.NParallel = atoiOr(r.FormValue("model_n_parallel"), cfg.Model.NParallel)
	cfg.Model.Port = atoiOr(r.FormValue("model_port"), cfg.Model.Port)
	cfg.Model.Verbose = r.FormValue("model_verbose") == "on"

	cfg.Embedder.Binary = strings.TrimSpace(r.FormValue("embed_binary"))
	cfg.Embedder.ModelPath = strings.TrimSpace(r.FormValue("embed_path"))
	cfg.Embedder.Port = atoiOr(r.FormValue("embed_port"), cfg.Embedder.Port)
	cfg.Embedder.Verbose = r.FormValue("embed_verbose") == "on"

	cfg.Memory.RepoPath = strings.TrimSpace(r.FormValue("memory_repo"))

	cfg.UI.Port = atoiOr(r.FormValue("ui_port"), cfg.UI.Port)
	cfg.UI.OpenOnStart = r.FormValue("ui_open_on_start") == "on"

	cfg.API.Enabled = r.FormValue("api_enabled") == "on"
	cfg.API.Port = atoiOr(r.FormValue("api_port"), cfg.API.Port)

	cfg.Prompt.CtxSize = atoiOr(r.FormValue("prompt_ctx_size"), cfg.Prompt.CtxSize)
	cfg.Prompt.MemoryTokenBudget = atoiOr(r.FormValue("prompt_memory_budget"), cfg.Prompt.MemoryTokenBudget)
	cfg.Prompt.ConversationReserve = atoiOr(r.FormValue("prompt_conversation_reserve"), cfg.Prompt.ConversationReserve)

	cfg.Queue.MaxDepth = atoiOr(r.FormValue("queue_max_depth"), cfg.Queue.MaxDepth)
	cfg.Queue.WALPath = strings.TrimSpace(r.FormValue("queue_wal_path"))

	cfg.Metrics.RetentionDays = atoiOr(r.FormValue("metrics_retention_days"), cfg.Metrics.RetentionDays)

	cfg.Log.RingMaxEntries = atoiOr(r.FormValue("log_ring_max_entries"), cfg.Log.RingMaxEntries)
	cfg.Log.ProcMaxLines = atoiOr(r.FormValue("log_proc_max_lines"), cfg.Log.ProcMaxLines)

	return &cfg
}

func atoiOr(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func stateToPayload(s stateSnapshot) ssePayload {
	errs := make([]string, 0, len(s.StartupErrors))
	for _, e := range s.StartupErrors {
		errs = append(errs, e.Error())
	}
	return ssePayload{
		LlamaHealthy:  s.LlamaStatus.Healthy,
		LlamaRunning:  s.LlamaStatus.Running,
		LlamaRestarts: s.LlamaStatus.RestartCount,
		LlamaFailed:   s.LlamaStatus.Failed,
		EmbedHealthy:  s.EmbedderStatus.Healthy,
		EmbedRunning:  s.EmbedderStatus.Running,
		EmbedRestarts: s.EmbedderStatus.RestartCount,
		EmbedFailed:   s.EmbedderStatus.Failed,
		QueueDepth:    s.QueueDepth,
		QueueMax:      s.QueueMax,
		StartupErrors: errs,
		FirstRun:      s.FirstRun,
		UptimeSeconds: int64(time.Since(s.StartTime).Seconds()),
	}
}
