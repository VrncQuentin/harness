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

	"github.com/VrncQuentin/harness/assets"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/logbuf"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/project"
)

// ProjectEditFunc is the Runtime-owned project-update surface used by the UI's
// /projects/edit handler. The runtime implements it so an edit serializes with
// the apply transaction and refuses to move the active project's memory repo;
// the UI never constructs and executes project.Workflow directly. main.go wires
// it to a closure over the runtime.
type ProjectEditFunc func(input project.UpdateInput, memoryRepoMode string) (project.Project, error)

// ServiceDeps is the set of UI-facing adapters owned by one runtime
// generation. The runtime binds an immutable ServiceDeps to each generation
// it publishes and hands it out through SnapshotProvider.AcquireUISnapshot,
// which also pins the generation so its readers and handles stay open until
// the returned release func is called. Handlers must use only the fields of
// one captured snapshot and must never reread individual live getters.
type ServiceDeps struct {
	MemoryRepoPath string

	// ActiveAgent is the active agent selection captured at snapshot
	// acquisition time, under the same runtime lock as the generation. It is
	// filled per request rather than frozen at generation build time because
	// /agents/active switches the active agent without rebuilding the
	// generation; chat/task handlers with an empty agent field fall back to
	// this value.
	ActiveAgent string

	AgentRegistry AgentRegistry
	MemoryStore   MemoryStore
	SessionStore  SessionStore

	Committer Committer
	Dedup     DedupChecker

	PromotionDedupThreshold float64
	RetrievalScorer         RetrievalScorer
	IndexRebuilder          IndexRebuilder

	ChatRunner ChatRunner
	TaskRunner TaskRunner
}

// SnapshotProvider hands out generation-bound UI dependency snapshots.
// AcquireUISnapshot atomically captures the current generation's complete
// snapshot and pins the generation so it cannot be retired (and its readers
// closed) before the returned release func is called. The concrete
// implementation lives in internal/runtime; the ui package only consumes it,
// keeping the import graph one-way.
type SnapshotProvider interface {
	AcquireUISnapshot() (ServiceDeps, func())
}

// RetryFunc is called when the user clicks Retry on the status page or saves
// a new config. The implementation (in main) is responsible for clearing and
// re-adding startup errors via the Server's methods, and returns what the
// reload achieved vs. what still needs a full harness restart.
type RetryFunc func() ApplyResult

// ApplyResult reports the outcome of re-reading + re-applying the saved config.
// LiveApplied is true when at least one component was reconfigured in place.
// RestartNeeded lists human-readable reasons (e.g. "UI port") for changes that
// require a full harness restart to take effect. Err is non-nil when the apply
// could not commit (config load, validation, or candidate-preparation
// failure); the live generation and recorded applied state are then untouched.
type ApplyResult struct {
	LiveApplied   bool
	RestartNeeded []string
	Err           error
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

// ProjectDirectoryWarning is an advisory status item for an unhealthy
// directory attached to the active project.
type ProjectDirectoryWarning struct {
	Path    string `json:"path"`
	Problem string `json:"problem"`
}

// stateSnapshot holds the copyable fields of State (no mutex).
type stateSnapshot struct {
	LlamaStatus              ProcessStatus
	EmbedderStatus           ProcessStatus
	QueueDepth               int
	QueueMax                 int
	StartupErrors            []error
	FirstRun                 bool
	StartTime                time.Time
	ProjectSlug              string
	ProjectDirectoryWarnings []ProjectDirectoryWarning
	ModelMismatch            bool
	LoadedModel              string
	PreferredModel           string
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

type uiDeps struct {
	retry RetryFunc

	llamaRestart func()
	embedRestart func()

	store        config.Store
	metricsStore metrics.Store
	projectStore ProjectStore

	projectEdit ProjectEditFunc

	binDir string

	projectNavSlugs []string
	projectNavNames map[string]string

	quit func()
}

func (d *uiDeps) copy() uiDeps {
	if d == nil {
		return uiDeps{}
	}
	next := *d
	next.projectNavSlugs = append([]string(nil), d.projectNavSlugs...)
	next.projectNavNames = copyStringMap(d.projectNavNames)
	return next
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Server is the UI HTTP server.
type Server struct {
	port  int
	state *State

	// sseClients maps chan string → struct{} for active SSE subscribers.
	sseClients sync.Map

	// chatSSEClients maps chan string → stream id for chat token subscribers.
	chatSSEClients sync.Map
	// taskSSEClients maps chan string → stream id for task event subscribers.
	taskSSEClients sync.Map

	serverCtxMu sync.RWMutex
	serverCtx   context.Context

	// Each page has its own template set because status.html, config.html,
	// and agents.html all define "title" and "content" - sharing one set
	// would let the later parse clobber the earlier one.
	statusTmpl            *template.Template
	configTmpl            *template.Template
	agentsTmpl            *template.Template
	chatTmpl              *template.Template
	memoryTmpl            *template.Template
	memoryEditTmpl        *template.Template
	memoryEpisodesTmpl    *template.Template
	memoryEpisodeViewTmpl *template.Template
	projectsTmpl          *template.Template
	taskTmpl              *template.Template
	// shutdownTmpl is intentionally standalone (no layout.html) so the
	// rendered page does not load /static/* — by the time the browser
	// fetches stylesheets the listener may already be gone.
	shutdownTmpl *template.Template

	// deps is swapped whole so handlers never observe a half-rebuilt mix of
	// control adapters while the runtime tears down and rewires services. It
	// holds only stable UI/control dependencies (retry, stores, restart
	// callbacks, navigation, quit). Generation-bound service adapters live in
	// the runtime generation and are reached through snapProvider.
	deps atomic.Pointer[uiDeps]

	// snapProvider hands out generation-bound UI snapshots with a lease.
	// Nil until the runtime wires itself, in which case handlers observe an
	// empty snapshot (setup CTA / service-unavailable responses).
	snapProvider atomic.Pointer[SnapshotProvider]

	logRing   atomic.Pointer[logbuf.Ring]
	llamaRing atomic.Pointer[logbuf.Ring]
	embedRing atomic.Pointer[logbuf.Ring]

	// quit is the callback that tears down the harness when the user
	// confirms the shutdown dialog. Wired by main to tray.Quit so the
	// /shutdown endpoint and the tray menu converge on one exit path.
	quitOnce sync.Once
}

// NewServer creates a new UI server on the given port. The config store is
// injected separately via SetConfigStore once the shared database is open.
func NewServer(port int) *Server {
	s := &Server{
		port:      port,
		state:     &State{data: stateSnapshot{StartTime: time.Now()}},
		serverCtx: context.Background(),
	}
	templates := parsePageTemplates(map[string][]string{
		"status": {
			"templates/layout.html",
			"templates/status.html",
		},
		"config": {
			"templates/layout.html",
			"templates/config.html",
		},
		"agents": {
			"templates/layout.html",
			"templates/agents.html",
		},
		"chat": {
			"templates/layout.html",
			"templates/chat.html",
		},
		"memory": {
			"templates/layout.html",
			"templates/memory.html",
		},
		"memory_edit": {
			"templates/layout.html",
			"templates/memory_edit.html",
		},
		"memory_episodes": {
			"templates/layout.html",
			"templates/memory_episodes.html",
		},
		"memory_episode_view": {
			"templates/layout.html",
			"templates/memory_episode_view.html",
		},
		"projects": {
			"templates/layout.html",
			"templates/projects.html",
			"templates/projects_create_form.html",
			"templates/projects_edit_form.html",
			"templates/projects_table.html",
		},
		"task": {
			"templates/layout.html",
			"templates/task.html",
		},
		"shutdown": {
			"templates/shutdown.html",
		},
	})
	s.statusTmpl = templates["status"]
	s.configTmpl = templates["config"]
	s.agentsTmpl = templates["agents"]
	s.chatTmpl = templates["chat"]
	s.memoryTmpl = templates["memory"]
	s.memoryEditTmpl = templates["memory_edit"]
	s.memoryEpisodesTmpl = templates["memory_episodes"]
	s.memoryEpisodeViewTmpl = templates["memory_episode_view"]
	s.projectsTmpl = templates["projects"]
	s.taskTmpl = templates["task"]
	s.shutdownTmpl = templates["shutdown"]
	return s
}

func (s *Server) depsSnapshot() uiDeps {
	return s.deps.Load().copy()
}

func (s *Server) updateDeps(mut func(*uiDeps)) {
	for {
		old := s.deps.Load()
		next := old.copy()
		mut(&next)
		if s.deps.CompareAndSwap(old, &next) {
			return
		}
	}
}

// SetSnapshotProvider installs the runtime's generation-bound snapshot
// provider. The provider is set once during runtime startup and is safe to
// call again on later reloads. Passing a nil provider stores a nil atomic
// pointer so acquisition returns the documented empty snapshot (setup CTA /
// service-unavailable responses) rather than panicking on a nil interface.
func (s *Server) SetSnapshotProvider(p SnapshotProvider) {
	if p == nil {
		s.snapProvider.Store(nil)
		return
	}
	s.snapProvider.Store(&p)
}

// acquireSnapshot captures the current generation-bound UI snapshot exactly
// once and pins the generation until the returned release func runs. Every
// handler that needs a generation-scoped dependency must acquire once and use
// only fields from the captured snapshot; it must not reread individual live
// getters, because those could span two publications.
func (s *Server) acquireSnapshot() (ServiceDeps, func()) {
	pp := s.snapProvider.Load()
	if pp == nil {
		return ServiceDeps{}, func() {}
	}
	return (*pp).AcquireUISnapshot()
}
func parsePageTemplates(pages map[string][]string) map[string]*template.Template {
	out := make(map[string]*template.Template, len(pages))
	for name, paths := range pages {
		out[name] = template.Must(template.ParseFS(assets.TemplateFS, paths...))
	}
	return out
}

// SetRetry installs the callback invoked on /retry and after a successful
// config save. Safe to call at any time; calls before it is set are no-ops.
func (s *Server) SetRetry(fn RetryFunc) {
	s.updateDeps(func(d *uiDeps) { d.retry = fn })
}

func (s *Server) callRetry() ApplyResult {
	fn := s.depsSnapshot().retry
	if fn == nil {
		return ApplyResult{}
	}
	return fn()
}

func (s *Server) hasRetry() bool {
	return s.depsSnapshot().retry != nil
}

// SetProcRestarts installs the callbacks used by the /procs/{name}/restart
// endpoints. Either may be nil while the matching manager is not yet up; the
// handler treats nil as a no-op and still redirects the user back to /.
func (s *Server) SetProcRestarts(llama, embed func()) {
	s.updateDeps(func(d *uiDeps) {
		d.llamaRestart = llama
		d.embedRestart = embed
	})
}

func (s *Server) getProcRestart(name string) func() {
	deps := s.depsSnapshot()
	switch name {
	case "llama":
		return deps.llamaRestart
	case "embed":
		return deps.embedRestart
	default:
		return nil
	}
}

// SetConfigStore installs the config store used by the /config page. If nil,
// the config page renders an error instead of a form.
func (s *Server) SetConfigStore(store config.Store) {
	s.updateDeps(func(d *uiDeps) { d.store = store })
}

func (s *Server) configStore() config.Store {
	return s.depsSnapshot().store
}

// SetMetricsStore installs the metrics store used by the Prometheus endpoint.
func (s *Server) SetMetricsStore(store metrics.Store) {
	s.updateDeps(func(d *uiDeps) { d.metricsStore = store })
}

func (s *Server) getMetricsStore() metrics.Store {
	return s.depsSnapshot().metricsStore
}

func (s *Server) SetProjectStore(store ProjectStore) {
	slugs, names := projectNavData(store)
	s.updateDeps(func(d *uiDeps) {
		d.projectStore = store
		d.projectNavSlugs = slugs
		d.projectNavNames = names
	})
}

func (s *Server) getProjectStore() ProjectStore {
	return s.depsSnapshot().projectStore
}

// SetProjectEditor installs the runtime-backed project-edit surface used by
// /projects/edit. Nil disables the editor; the handler then redirects with a
// clear error instead of mutating the project store directly.
func (s *Server) SetProjectEditor(fn ProjectEditFunc) {
	s.updateDeps(func(d *uiDeps) { d.projectEdit = fn })
}

func (s *Server) getProjectEditor() ProjectEditFunc {
	return s.depsSnapshot().projectEdit
}
func (s *Server) refreshProjectNav() {
	store := s.getProjectStore()
	slugs, names := projectNavData(store)
	s.updateDeps(func(d *uiDeps) {
		d.projectNavSlugs = slugs
		d.projectNavNames = names
	})
}

func projectNavData(store ProjectStore) ([]string, map[string]string) {
	var slugs []string
	names := make(map[string]string)
	if store != nil {
		projects, err := store.List(false)
		if err == nil {
			slugs = make([]string, 0, len(projects))
			for _, p := range projects {
				slugs = append(slugs, p.Slug)
				names[p.Slug] = p.DisplayName
			}
		}
	}
	return slugs, names
}
func (s *Server) projectNavSnapshot() ([]string, map[string]string) {
	deps := s.depsSnapshot()
	return deps.projectNavSlugs, deps.projectNavNames
}

// SetBinDir records the directory containing the running harness binary. It is
// used by the /config page to suggest detected llama-server and .gguf paths.
// Safe to leave unset; detection then returns no suggestions.
func (s *Server) SetBinDir(dir string) {
	s.updateDeps(func(d *uiDeps) { d.binDir = dir })
}

func (s *Server) getBinDir() string {
	return s.depsSnapshot().binDir
}

// SetQuit installs the callback invoked when the user confirms the shutdown
// dialog. Safe to call before or after Start; calls before it is set return
// 503 from /shutdown so the user sees a clear failure rather than a no-op.
func (s *Server) SetQuit(fn func()) {
	s.updateDeps(func(d *uiDeps) { d.quit = fn })
}

func (s *Server) getQuit() func() {
	return s.depsSnapshot().quit
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

// SetProjectDirectoryWarnings updates advisory health warnings for the active
// project's configured directories. These warnings do not block startup.
func (s *Server) SetProjectDirectoryWarnings(slug string, warnings []ProjectDirectoryWarning) {
	copied := make([]ProjectDirectoryWarning, len(warnings))
	copy(copied, warnings)

	s.state.mu.Lock()
	s.state.data.ProjectSlug = slug
	s.state.data.ProjectDirectoryWarnings = copied
	s.state.mu.Unlock()
	s.broadcastState()
}

// ModelMismatch returns the current model-mismatch indicator set by the
// runtime: whether the running model differs from the active project's
// preferred model, plus the two model paths. Tests use it to assert the status
// UI represents the mismatch honestly.
func (s *Server) ModelMismatch() (bool, string, string) {
	snap := s.state.snapshot()
	return snap.ModelMismatch, snap.LoadedModel, snap.PreferredModel
}

// SetModelMismatch updates whether the currently loaded model differs from
// the active project's preferred model (relevant when llama_on_switch=keep).
// Empty strings clear the mismatch indicator.
func (s *Server) SetModelMismatch(mismatch bool, loaded, preferred string) {
	s.state.mu.Lock()
	s.state.data.ModelMismatch = mismatch
	s.state.data.LoadedModel = loaded
	s.state.data.PreferredModel = preferred
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
// originPolicy applies the browser boundary once for every state-changing
// request and for the log-bearing event stream. Requests without Origin are
// retained for local navigation; cross-origin browser requests are rejected
// before reaching individual handlers.
func (s *Server) originPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodPost || isProtectedEventStream(r.URL.Path)) && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isProtectedEventStream(path string) bool {
	switch path {
	case "/events", "/chat/events", "/task/events":
		return true
	default:
		return false
	}
}
func (s *Server) Start(ctx context.Context) error {
	s.serverCtxMu.Lock()
	s.serverCtx = ctx
	s.serverCtxMu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatus)
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/config", s.handleConfig)
	mux.HandleFunc("/agents", s.handleAgents)
	mux.HandleFunc("/agents/active", s.handleAgentsActive)
	mux.HandleFunc("/agents/create", s.handleAgentsCreate)
	mux.HandleFunc("/agents/delete", s.handleAgentsDelete)
	mux.HandleFunc("/agents/persona", s.handleAgentsPersona)
	mux.HandleFunc("/agents/rules", s.handleAgentsRules)
	mux.HandleFunc("/agents/notes", s.handleAgentsNotes)
	mux.HandleFunc("/chat", s.handleChat)
	mux.HandleFunc("/chat/events", s.handleChatEvents)
	mux.HandleFunc("/chat/send", s.handleChatSend)

	mux.HandleFunc("/chat/save", s.handleChatSave)
	mux.HandleFunc("/chat/session", s.handleChatSessionResume)
	mux.HandleFunc("/memory", s.handleMemory)
	mux.HandleFunc("/memory/edit", s.handleMemoryEdit)
	mux.HandleFunc("/memory/save", s.handleMemorySave)
	mux.HandleFunc("/memory/episodes", s.handleMemoryEpisodes)
	mux.HandleFunc("/memory/episodes/view", s.handleMemoryEpisodeView)
	mux.HandleFunc("/memory/promote", s.handlePromoteFact)
	mux.HandleFunc("/memory/note", s.handleAppendNote)
	mux.HandleFunc("/projects", s.handleProjects)
	mux.HandleFunc("/projects/activate", s.handleProjectActivate)
	mux.HandleFunc("/projects/hide", s.handleProjectHide)
	mux.HandleFunc("/projects/unhide", s.handleProjectUnhide)
	mux.HandleFunc("/projects/edit", s.handleProjectEdit)
	mux.HandleFunc("/task", s.handleTask)
	mux.HandleFunc("/task/events", s.handleTaskEvents)
	mux.HandleFunc("/task/send", s.handleTaskSend)
	mux.HandleFunc("/task/approval", s.handleTaskApproval)
	mux.HandleFunc("/task/cancel", s.handleTaskCancel)
	mux.HandleFunc("/retry", s.handleRetry)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/memory/scaffold", s.handleMemoryScaffold)
	mux.HandleFunc("/memory/rebuild-index", s.handleMemoryRebuildIndex)
	mux.HandleFunc("/procs/llama/restart", s.handleProcRestart("llama"))
	mux.HandleFunc("/procs/embed/restart", s.handleProcRestart("embed"))
	mux.HandleFunc("/shutdown", s.handleShutdown)
	mux.Handle("/static/", http.FileServer(http.FS(assets.StaticFS)))

	srv := &http.Server{
		// Loopback only. The management UI has no authentication layer and
		// exposes state-changing routes (/config, /shutdown, /task/send).
		// originPolicy blocks cross-origin browser requests, but it cannot
		// stop a non-browser client that simply omits the Origin header, so
		// the bind address is the boundary that actually holds.
		Addr:    fmt.Sprintf("127.0.0.1:%d", s.port),
		Handler: s.originPolicy(mux),
	}

	// Verify we can bind before returning.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("ui: bind %s: %w", srv.Addr, err)
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

func (s *Server) asyncContext() context.Context {
	s.serverCtxMu.RLock()
	ctx := s.serverCtx
	s.serverCtxMu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// basePage holds template fields shared by every rendered page (nav highlight
// and footer uptime live in layout.html).
type basePage struct {
	Page              string
	UptimeText        string
	ActiveProjectSlug string
	ActiveProjectName string
	ProjectSlugs      []string
	ProjectNames      map[string]string
	// GlobalSlug is the reserved project slug rendered in the sidebar. Its
	// gear links to /config instead of a per-project edit form.
	GlobalSlug string
}

func (s *Server) newBasePage(page string) basePage {
	snap := s.state.snapshot()
	slugs, names := s.projectNavSnapshot()
	bp := basePage{
		Page:              page,
		UptimeText:        formatUptime(time.Since(snap.StartTime)),
		ActiveProjectSlug: snap.ProjectSlug,
		ProjectSlugs:      slugs,
		ProjectNames:      names,
		GlobalSlug:        project.GlobalSlug,
	}
	if bp.ActiveProjectSlug == "" {
		bp.ActiveProjectSlug = "global"
	}
	if bp.ProjectNames != nil {
		bp.ActiveProjectName = bp.ProjectNames[bp.ActiveProjectSlug]
	}
	if bp.ActiveProjectName == "" {
		bp.ActiveProjectName = "Global"
	}
	return bp
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
