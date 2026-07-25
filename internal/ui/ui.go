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
)

// ServiceDeps is the set of UI-facing adapters produced by the runtime memory
// and API service graph. SetServiceDeps publishes these as one immutable
// snapshot so handlers never observe a half-rebuilt mix of old and new
// adapters while config/project changes are being applied.
type ServiceDeps struct {
	MemoryRepoPath string

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

	agentReg AgentRegistry
	binDir   string

	// memRepo is the active project memory repo path. Empty means
	// project memory is not available yet, in which case the status page
	// suppresses the layout-scaffolding prompt entirely.
	memRepo  string
	memStore MemoryStore

	committer Committer
	dedup     DedupChecker

	promotionDedupThreshold float64
	scorer                  RetrievalScorer
	rebuilder               IndexRebuilder

	chatRunner   ChatRunner
	taskRunner   TaskRunner
	sessionStore SessionStore

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
	// adapters while the runtime tears down and rewires memory/API services.
	deps atomic.Pointer[uiDeps]

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
func parsePageTemplates(pages map[string][]string) map[string]*template.Template {
	out := make(map[string]*template.Template, len(pages))
	for name, paths := range pages {
		out[name] = template.Must(template.ParseFS(assets.TemplateFS, paths...))
	}
	return out
}

// SetServiceDeps publishes the runtime-owned UI adapters as one snapshot. Pass
// a zero value to detach them while the memory/API service graph is stopped or
// invalid.
func (s *Server) SetServiceDeps(deps ServiceDeps) {
	s.updateDeps(func(d *uiDeps) {
		d.memRepo = deps.MemoryRepoPath
		d.agentReg = deps.AgentRegistry
		d.memStore = deps.MemoryStore
		d.sessionStore = deps.SessionStore
		d.committer = deps.Committer
		d.dedup = deps.Dedup
		d.promotionDedupThreshold = deps.PromotionDedupThreshold
		d.scorer = deps.RetrievalScorer
		d.rebuilder = deps.IndexRebuilder
		d.chatRunner = deps.ChatRunner
		d.taskRunner = deps.TaskRunner
	})
}

// ServiceDepsSnapshot returns the currently published runtime service adapters.
// Runtime uses it to restore a known-good UI graph if a live reload fails after
// detaching services.
func (s *Server) ServiceDepsSnapshot() ServiceDeps {
	deps := s.depsSnapshot()
	return ServiceDeps{
		MemoryRepoPath:          deps.memRepo,
		AgentRegistry:           deps.agentReg,
		MemoryStore:             deps.memStore,
		SessionStore:            deps.sessionStore,
		Committer:               deps.committer,
		Dedup:                   deps.dedup,
		PromotionDedupThreshold: deps.promotionDedupThreshold,
		RetrievalScorer:         deps.scorer,
		IndexRebuilder:          deps.rebuilder,
		ChatRunner:              deps.chatRunner,
		TaskRunner:              deps.taskRunner,
	}
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

func (s *Server) getMemoryRepoPath() string {
	return s.depsSnapshot().memRepo
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
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: s.originPolicy(mux),
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
