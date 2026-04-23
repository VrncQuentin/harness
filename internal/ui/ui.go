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
	"time"

	"github.com/vrnc/harness/assets"
	"github.com/vrnc/harness/internal/config"
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
	StderrTail   []string
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

	storeMu sync.RWMutex
	store   config.Store

	binDirMu sync.RWMutex
	binDir   string
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

// SetQueueDepth updates the queue depth.
func (s *Server) SetQueueDepth(depth, max int) {
	s.state.mu.Lock()
	s.state.data.QueueDepth = depth
	s.state.data.QueueMax = max
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
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/retry", s.handleRetry)
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

func queuePct(depth, max int) int {
	if max <= 0 {
		return 0
	}
	p := depth * 100 / max
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

	ch := make(chan string, 8)
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
		ch := key.(chan string)
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
	EmbedHealthy  bool     `json:"embed_healthy"`
	EmbedRunning  bool     `json:"embed_running"`
	EmbedRestarts int      `json:"embed_restarts"`
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
		s.renderConfig(w, r, configPageData{}, false)
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
		s.renderConfig(w, r, configPageData{SaveErr: "could not parse form: " + err.Error()}, true)
		return
	}

	store := s.configStore()
	if store == nil {
		s.renderConfig(w, r, configPageData{SaveErr: "config store unavailable"}, true)
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
		s.renderConfig(w, r, configPageData{Config: cfg, ValidationErr: err.Error()}, true)
		return
	}
	if err := store.Save(cfg); err != nil {
		s.renderConfig(w, r, configPageData{Config: cfg, SaveErr: err.Error()}, true)
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

	cfg.Embedder.Binary = strings.TrimSpace(r.FormValue("embed_binary"))
	cfg.Embedder.ModelPath = strings.TrimSpace(r.FormValue("embed_path"))
	cfg.Embedder.Port = atoiOr(r.FormValue("embed_port"), cfg.Embedder.Port)

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
		EmbedHealthy:  s.EmbedderStatus.Healthy,
		EmbedRunning:  s.EmbedderStatus.Running,
		EmbedRestarts: s.EmbedderStatus.RestartCount,
		QueueDepth:    s.QueueDepth,
		QueueMax:      s.QueueMax,
		StartupErrors: errs,
		FirstRun:      s.FirstRun,
		UptimeSeconds: int64(time.Since(s.StartTime).Seconds()),
	}
}
