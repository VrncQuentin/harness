package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/agentloop"
	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/approvals"
	"github.com/VrncQuentin/harness/internal/embedder"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/governor"
	"github.com/VrncQuentin/harness/internal/home"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/memoryops"
	"github.com/VrncQuentin/harness/internal/metrics"
	"github.com/VrncQuentin/harness/internal/parser"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/prompt"
	"github.com/VrncQuentin/harness/internal/session"
	"github.com/VrncQuentin/harness/internal/tools"
	"github.com/VrncQuentin/harness/internal/ui"
)

type episodeScoreService interface {
	ScoreEpisodes(ctx context.Context, query string, episodePaths []string) (map[string]memoryops.RetrievalScore, error)
}

type uiRetrievalScorerAdapter struct {
	scorer episodeScoreService
}

func (a *uiRetrievalScorerAdapter) ScoreEpisodes(ctx context.Context, query string, episodePaths []string) (map[string]ui.RetrievalScore, error) {
	out := make(map[string]ui.RetrievalScore, len(episodePaths))
	for _, p := range episodePaths {
		out[p] = ui.RetrievalScore{}
	}
	if a == nil || a.scorer == nil {
		return out, nil
	}
	scores, err := a.scorer.ScoreEpisodes(ctx, query, episodePaths)
	if err != nil {
		return out, err
	}
	for p, score := range scores {
		out[p] = ui.RetrievalScore{
			Indexed:  score.Indexed,
			Score:    score.Score,
			HasScore: score.HasScore,
		}
	}
	return out, nil
}

// startMemoryAndAPI brings up the memory reader, agent registry, prompt
// assembler, hot-reload watcher, session manager, and API server.
// Caller must hold rt.mu.
//
// metricsStore may be nil; the session manager simply skips metric
// emission in that case.
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store) (started bool) {
	// Everything this generation of the memory graph pins is collected as it is
	// opened, so a start that gives up partway closes exactly what it took and
	// a rebuild closes exactly the generation it replaced.
	var owned memoryHandles
	defer func() {
		if !started {
			owned.close()
		}
	}()

	roots, err := rt.resolveProjectRepoRootsForSlug(rt.cfg.Project.ActiveProjectSlug)
	if err != nil {
		uiServer.SetServiceDeps(ui.ServiceDeps{})
		uiServer.AddStartupError(fmt.Errorf("project memory repos: %w", err))
		if rt.cfg.API.Enabled {
			uiServer.AddStartupError(errors.New("api server disabled: project memory repos are not valid"))
		}
		return false
	}

	svcDeps := ui.ServiceDeps{MemoryRepoPath: roots.activeRoot}

	// Validation and the long-lived pin are one call, not two: OpenValidatedDirReader
	// pins root once and checks the layout through that same handle, then
	// hands the reader back still holding it. Validating with one call and
	// opening the reader with a second, separate call — as this used to do —
	// would leave a window between them in which root can change, so the
	// layout that was validated and the repo the reader ends up bound to are
	// not provably the same directory.
	owned.globalMem, err = memory.OpenValidatedDirReader(roots.globalRoot, true)
	if err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("global memory repo: %w", err))
		return false
	}
	if roots.activeSlug == project.GlobalSlug {
		// One repo reached by two names would otherwise be pinned twice. Both
		// handles would be valid; taking a second one only doubles what has to
		// be closed on every reload path.
		owned.activeMem = owned.globalMem
	} else {
		owned.activeMem, err = memory.OpenValidatedDirReader(roots.activeRoot, false)
		if err != nil {
			uiServer.SetServiceDeps(svcDeps)
			uiServer.AddStartupError(fmt.Errorf("active memory repo: %w", err))
			return false
		}
	}
	rt.globalMem = owned.globalMem
	rt.activeMem = owned.activeMem
	rt.agentReg = agent.NewDiskRegistry(rt.globalMem, rt.getActiveAgent, rt.setActiveAgent)
	rt.assembler = prompt.NewProjectDiskAssembler(rt.globalMem, rt.activeMem, rt.agentReg, rt.effectivePromptFor(&rt.cfg)).WithProjectSlug(rt.cfg.Project.ActiveProjectSlug)

	// The project-scoped service owns one index handle for prompt retrieval,
	// scoring, save hooks, and rebuilding. Missing indexes are created lazily;
	// malformed indexes surface as setup errors instead of being discarded.
	//
	// It is located by a repo-relative name resolved through the active repo's
	// own handle, never by an absolute "<repo>/index/_episodes" pathname: that
	// name can already lead out of the repo, and pinning it would then pin
	// somewhere else entirely.
	episodeIndex, err := memoryops.NewEpisodeIndex(owned.activeMem, memoryops.EpisodeIndexRootRel)
	if err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("episode index: %w", err))
		return false
	}
	owned.episodes = episodeIndex
	embedClient := rt.newEmbedderClient()
	rt.assembler = rt.assembler.WithBlendedRetrieval(episodeIndex, embedClient)
	svcDeps.RetrievalScorer = &uiRetrievalScorerAdapter{scorer: &memoryops.EpisodeScorer{
		Embedder: embedClient,
		Config:   rt.cfg.Prompt,
		Index:    episodeIndex,
	}}
	svcDeps.MemoryStore = rt.activeMem
	svcDeps.AgentRegistry = &uiAgentRegistryAdapter{reg: rt.agentReg, globalMem: rt.globalMem, activeMem: rt.activeMem, getProjectSlug: rt.getActiveProjectSlug, setActive: rt.setActiveAgent}

	// Session manager is layered on top of the validated memory repo.
	// A failure to open the git repo surfaces as a startup error and
	// silently disables save/resume so the rest of the harness stays
	// usable.
	sessionMgr, sessionAdapter := rt.buildSessionManagerWithClients(metricsStore, uiServer, roots, owned.activeMem, episodeIndex, rt.ensureInferenceClient(), embedClient)
	rt.setSessionManager(sessionMgr)
	if sessionAdapter != nil {
		svcDeps.SessionStore = sessionAdapter
	}

	// Wire active-project fact/note promotion and deduplication.
	if rt.gitRepo != nil {
		svcDeps.Committer = rt.gitRepo
	}
	svcDeps.Dedup = &memoryops.DedupChecker{
		Mem:      rt.activeMem,
		Embedder: embedClient,
	}
	svcDeps.PromotionDedupThreshold = rt.cfg.Prompt.PromotionDedupThreshold

	asmAdapter := &apiAssemblerAdapter{rt: rt}
	svcDeps.IndexRebuilder = &memoryops.EpisodeRebuilder{
		Mem:      rt.activeMem,
		Embedder: embedClient,
		Index:    episodeIndex,
		Repo:     rt.gitRepo,
	}
	if rt.reqQueue != nil {
		svcDeps.ChatRunner = &chatRunnerAdapter{
			asm: asmAdapter,
			q:   rt.reqQueue,
			mgr: sessionMgr,
		}
	}

	if rt.cfg.API.Enabled && rt.reqQueue != nil {
		var apiRec api.SessionRecorder
		if sessionMgr != nil {
			apiRec = &apiSessionAdapter{mgr: sessionMgr}
		}
		srv := api.NewServer(rt.cfg.API.Port, asmAdapter, rt.reqQueue, apiRec)
		if err := srv.Start(ctx); err != nil {
			uiServer.AddStartupError(fmt.Errorf("api server: %w", err))
		} else {
			rt.apiServer = srv
			slog.Info("api server listening", "port", rt.cfg.API.Port)
		}
	}

	// Wire the task runner with assembler + queue.
	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry); err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("task tools: %w", err))
		return false
	}

	// Build the governor (B1 + B3). Failures to resolve the harness home or
	// create the parser registry degrade gracefully — the governor is omitted
	// rather than blocking the rest of startup.
	var gov agentloop.Governor
	var tooloutDir string
	if harnessHome, err := home.Default(); err == nil {
		cacheDir := filepath.Join(harnessHome, "cache")
		// Resolved from the same cache dir the governor spills into, so the
		// handles B3 emits and the directory read resolves them against cannot
		// drift apart.
		tooloutDir = governor.TooloutDir(cacheDir)
		if parsers, err := parser.NewRegistry(parser.NewGoFrontEnd()); err == nil {
			gov = governor.New(parsers, cacheDir)
		}
	}

	// Build the permission base layers. Each task engine gets a fresh
	// evaluator so mutable session approval rules stay scoped to that session.
	loopCfg := rt.cfg.Loop
	userLayer := approvals.Layer{Name: "user-config"}
	if !loopCfg.EditEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "edit", Decision: approvals.Denied, Source: "user: edit disabled in config",
		})
	}
	if !loopCfg.ExecEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "exec", Decision: approvals.Denied, Source: "user: exec disabled in config",
		})
	}
	if !loopCfg.GoTestEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "go_test", Decision: approvals.Denied, Source: "user: go_test disabled in config",
		})
	}
	if !loopCfg.GoLintEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "go_lint", Decision: approvals.Denied, Source: "user: go_lint disabled in config",
		})
	}
	if !loopCfg.GitCommitEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "git_commit", Decision: approvals.Denied, Source: "user: git_commit disabled in config",
		})
	}
	if !loopCfg.GitBranchEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "git_branch", Decision: approvals.Denied, Source: "user: git_branch disabled in config",
		})
	}
	if !loopCfg.GitCheckoutEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "git_checkout", Decision: approvals.Denied, Source: "user: git_checkout disabled in config",
		})
	}
	if !loopCfg.WebSearchEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "web_search", Decision: approvals.Denied, Source: "user: web_search disabled in config",
		})
	}
	if !loopCfg.MemoryQueryEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "memory_query", Decision: approvals.Denied, Source: "user: memory_query disabled in config",
		})
	}
	if !loopCfg.GitPushEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "git_push", Decision: approvals.Denied, Source: "user: git_push disabled in config",
		})
	}
	if !loopCfg.GHPRCreateEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "gh_pr_create", Decision: approvals.Denied, Source: "user: gh_pr_create disabled in config",
		})
	}
	if !loopCfg.GHPRMergeEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "gh_pr_merge", Decision: approvals.Denied, Source: "user: gh_pr_merge disabled in config",
		})
	}
	if !loopCfg.GHPRWaitEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "gh_pr_wait", Decision: approvals.Denied, Source: "user: gh_pr_wait disabled in config",
		})
	}
	approvalLayers := []approvals.Layer{approvals.DefaultLayer(), userLayer}

	var loopMetrics agentloop.MetricsRecorder
	if metricsStore != nil {
		loopMetrics = metrics.NewRecorder(metricsStore)
	}
	taskAdapter := &taskRunnerAdapter{
		rt:             rt,
		registry:       registry,
		asm:            asmAdapter,
		q:              rt.reqQueue,
		memScorer:      &memoryops.EpisodeScorer{Embedder: embedClient, Config: rt.cfg.Prompt, Index: episodeIndex},
		approvalLayers: approvalLayers,
		metrics:        loopMetrics,
		gov:            gov,
		tooloutDir:     tooloutDir,
	}
	rt.taskRunner = taskAdapter
	svcDeps.TaskRunner = taskAdapter
	uiServer.SetServiceDeps(svcDeps)
	// Transfer ownership: from here the runtime closes these, and the deferred
	// cleanup above no longer applies because the start succeeded.
	rt.memHandles = owned
	return true
}

func (rt *Runtime) memoryAPIUnavailable() bool {
	return rt.globalMem == nil ||
		rt.activeMem == nil ||
		rt.agentReg == nil ||
		rt.assembler == nil ||
		rt.taskRunner == nil ||
		rt.SessionManager() == nil ||
		(rt.cfg.API.Enabled && rt.apiServer == nil)
}

// memoryHandles collects the OS handles one generation of the memory service
// graph pins for its lifetime.
//
// They are gathered in one place because the reload path replaces the whole
// graph at once and can still fall back to the previous one: the handles being
// replaced must stay open until the replacement is known to have started, and
// must then be closed exactly once. Closing them inside stopMemoryAndAPI would
// be too early — that runs before the new graph is built, and a failed build
// restores the old references, which would then be closed handles.
type memoryHandles struct {
	globalMem *memory.DirReader
	activeMem *memory.DirReader
	episodes  *memoryops.EpisodeIndex
}

func (h memoryHandles) close() {
	if h.episodes != nil {
		if err := h.episodes.Close(); err != nil {
			slog.Warn("closing episode index", "err", err)
		}
	}
	if h.activeMem != nil && h.activeMem != h.globalMem {
		if err := h.activeMem.Close(); err != nil {
			slog.Warn("closing active memory repo", "err", err)
		}
	}
	if h.globalMem != nil {
		if err := h.globalMem.Close(); err != nil {
			slog.Warn("closing global memory repo", "err", err)
		}
	}
}

type memoryAPISnapshot struct {
	owned       memoryHandles
	globalMem   memory.Repo
	activeMem   memory.Repo
	agentReg    *agent.DiskRegistry
	assembler   *prompt.DiskAssembler
	apiServer   *api.Server
	gitRepo     *gitw.Repo
	sessionMgr  *session.Manager
	taskRunner  *taskRunnerAdapter
	serviceDeps ui.ServiceDeps
}

func (rt *Runtime) snapshotMemoryAndAPI(uiServer *ui.Server) memoryAPISnapshot {
	return memoryAPISnapshot{
		owned:       rt.memHandles,
		globalMem:   rt.globalMem,
		activeMem:   rt.activeMem,
		agentReg:    rt.agentReg,
		assembler:   rt.assembler,
		apiServer:   rt.apiServer,
		gitRepo:     rt.gitRepo,
		sessionMgr:  rt.SessionManager(),
		taskRunner:  rt.taskRunner,
		serviceDeps: uiServer.ServiceDepsSnapshot(),
	}
}

// closeReplaced releases the handles a snapshot was holding. It is called only
// once the replacement graph has started, because until then the snapshot is
// still the fallback.
func (snap memoryAPISnapshot) closeReplaced() { snap.owned.close() }

func (rt *Runtime) restoreMemoryAndAPI(uiServer *ui.Server, snap memoryAPISnapshot) {
	rt.memHandles = snap.owned
	rt.globalMem = snap.globalMem
	rt.activeMem = snap.activeMem
	rt.agentReg = snap.agentReg
	rt.assembler = snap.assembler
	rt.apiServer = snap.apiServer
	rt.gitRepo = snap.gitRepo
	rt.setSessionManager(snap.sessionMgr)
	rt.taskRunner = snap.taskRunner
	uiServer.SetServiceDeps(snap.serviceDeps)
}

type projectRepoRoots struct {
	globalRoot string
	activeRoot string
	activeSlug string
}

func (rt *Runtime) resolveProjectRepoRootsForSlug(slug string) (projectRepoRoots, error) {
	store := rt.projectStore
	if store == nil {
		return projectRepoRoots{}, errors.New("project store not available")
	}
	globalProj, err := store.Get(project.GlobalSlug)
	if err != nil {
		return projectRepoRoots{}, fmt.Errorf("global project: %w", err)
	}
	if slug == "" {
		slug = project.GlobalSlug
	}
	activeProj := globalProj
	if slug != project.GlobalSlug {
		activeProj, err = store.Get(slug)
		if err != nil {
			return projectRepoRoots{}, fmt.Errorf("active project %q: %w", slug, err)
		}
	}
	if strings.TrimSpace(globalProj.MemoryRepoPath) == "" {
		return projectRepoRoots{}, errors.New("global project memory repo path is empty")
	}
	if strings.TrimSpace(activeProj.MemoryRepoPath) == "" {
		return projectRepoRoots{}, fmt.Errorf("active project %q memory repo path is empty", slug)
	}
	return projectRepoRoots{
		globalRoot: globalProj.MemoryRepoPath,
		activeRoot: activeProj.MemoryRepoPath,
		activeSlug: slug,
	}, nil
}

// buildSessionManagerWithClients wires the session manager onto the already
// pinned active-project repo handle. sessionStore is that handle; episodeIndex
// is the one shared index for the project.
func (rt *Runtime) buildSessionManagerWithClients(
	metricsStore metrics.Store,
	uiServer *ui.Server,
	roots projectRepoRoots,
	sessionStore memory.Repo,
	episodeIndex *memoryops.EpisodeIndex,
	infClient inference.Client,
	embedClient embedder.Client,
) (*session.Manager, *uiSessionStoreAdapter) {
	repo, err := gitw.Open(roots.activeRoot)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("session manager: %w", err))
		return nil, nil
	}
	// go-git addresses its storage by pathname and has no handle to bind to
	// sessionStore's own pin, so this is the closest check available: both
	// sides' identities are compared as already-resolved values, each taken at
	// its own open time, rather than by re-resolving roots.activeRoot a third
	// time here — a fresh resolution at this later point would only confirm
	// what the path currently names, not whether it named the same thing when
	// go-git opened it moments before. dr is nil for a test fake that does not
	// implement Identity; production always passes the real *memory.DirReader.
	if dr, ok := sessionStore.(*memory.DirReader); ok {
		if !dr.Identity().Equal(repo.Identity()) {
			uiServer.AddStartupError(errors.New("session manager: git repository does not match the pinned memory repo"))
			return nil, nil
		}
	}
	rt.gitRepo = repo

	var rec session.MetricsRecorder
	if metricsStore != nil {
		rec = metrics.NewRecorder(metricsStore)
	}

	// The session manager reads and writes through the repo handle already
	// pinned for the active project rather than opening a second one on the
	// same path. Two handles would be two chances for the name to have meant
	// something different, and the log the manager appends to has to be the
	// same file the memory reader serves.
	mgr, err := session.NewManager(session.ManagerDeps{
		Repo:             repo,
		Writer:           sessionStore,
		Reader:           sessionStore,
		Appender:         sessionStore,
		Inference:        infClient,
		Metrics:          rec,
		SummarizerPrompt: rt.summarizerPromptFn(),
		AfterSave:        memoryops.AfterSaveEmbed(embedClient, episodeIndex, rt.gitRepo),
	}, rt.cfg.Project.ActiveProjectSlug)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("session manager: %w", err))
		return nil, nil
	}
	adapter := &uiSessionStoreAdapter{mgr: mgr, getActive: rt.getActiveAgent}
	return mgr, adapter
}

// summarizerPromptFn returns a getter that reads the live config
// SummarizerPrompt under the runtime mutex so /config edits propagate
// without rebuilding the manager.
func (rt *Runtime) summarizerPromptFn() session.SummarizerPromptFunc {
	return func() string {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.cfg.Prompt.SummarizerPrompt
	}
}

// setSessionManager swaps the live manager under its dedicated mutex
// so callers off the main reload path (chat handler, shutdown) can read
// without contending with config reloads.
func (rt *Runtime) setSessionManager(mgr *session.Manager) {
	rt.sessionMu.Lock()
	rt.sessionMg = mgr
	rt.sessionMu.Unlock()
}

// SessionManager exposes the live session manager for test code and
// external callers (cmd/harness shutdown). Returns nil when the repo
// has not been validated yet.
func (rt *Runtime) SessionManager() *session.Manager {
	rt.sessionMu.RLock()
	defer rt.sessionMu.RUnlock()
	return rt.sessionMg
}

// quiesceMemoryAndAPI cancels active task loops, flushes live sessions, and
// waits for in-flight UI requests tracked by trackGenRequest before a
// memory/API rebuild drops the current generation's handles. Caller must hold
// rt.mu on entry; the method releases it while waiting so session
// summarization can read live config through summarizerPromptFn without
// deadlocking, and so a tracked UI request blocked on rt.mu elsewhere can
// finish rather than deadlock against this call.
//
// The task-cancel and session-flush steps only log a warning on failure —
// neither one guards a safety property the way the UI drain does, so a slow
// task or a stuck flush does not by itself block a reload. The UI drain is
// different: it leaves uiServer's generation gate closed to new admissions
// on success (see Server.DrainGenerationRequests), and the caller must call
// uiServer.ResumeGenerationAdmission once it has finished swapping in the new
// generation or restoring the old one. On error, the gate has already
// reopened itself and nothing has been quiesced on the UI side — the caller
// must abort the rebuild and keep using the current generation rather than
// proceed to close its handles.
//
// This always runs the drain, even when there is no task loop or session
// manager yet: skipping it in that case would leave a request that raced in
// through a trackGenRequest handler unaccounted for.
func (rt *Runtime) quiesceMemoryAndAPI(ctx context.Context, uiServer *ui.Server) error {
	tasks := rt.taskRunner
	mgr := rt.SessionManager()

	rt.mu.Unlock()
	defer rt.mu.Lock()

	if tasks != nil {
		cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := tasks.CancelAll(cancelCtx); err != nil {
			slog.Warn("runtime reload: task loop shutdown wait", "err", err)
		}
		cancel()
	}
	if mgr != nil {
		flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := mgr.FlushAll(flushCtx); err != nil {
			slog.Warn("runtime reload: session flush", "err", err)
		}
		cancel()
	}
	if uiServer == nil {
		return nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := uiServer.DrainGenerationRequests(drainCtx); err != nil {
		return fmt.Errorf("UI request drain: %w", err)
	}
	return nil
}

// stopMemoryAndAPI tears down memory, sessions, task, and API services. Caller
// must hold rt.mu and should call quiesceMemoryAndAPI first when replacing live
// services during reload.
//
// It drops references and does not close the pinned repo handles. Ownership of
// those belongs to whoever took the snapshot: on the reload path the previous
// generation stays open until the replacement has started, because a failed
// start restores it.
func (rt *Runtime) stopMemoryAndAPI(uiServer *ui.Server) {
	if rt.apiServer != nil {
		rt.apiServer.Stop()
		rt.apiServer = nil
	}
	rt.memHandles = memoryHandles{}
	rt.globalMem = nil
	rt.activeMem = nil
	rt.agentReg = nil
	rt.assembler = nil
	rt.gitRepo = nil
	rt.setSessionManager(nil)
	uiServer.SetServiceDeps(ui.ServiceDeps{})
	rt.taskRunner = nil
}

func (rt *Runtime) getActiveAgent() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg.Agent.Active
}

func (rt *Runtime) getActiveProjectSlug() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	slug := rt.cfg.Project.ActiveProjectSlug
	if slug == "" {
		slug = "global"
	}
	return slug
}

func (rt *Runtime) getAssembler() *prompt.DiskAssembler {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.assembler
}

func (rt *Runtime) setActiveAgent(name string) error {
	rt.mu.Lock()
	store := rt.cfgStore
	rt.mu.Unlock()

	if store == nil {
		return ErrConfigStoreUnavailable
	}
	loaded, _, err := store.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	loaded.Agent.Active = name
	if err := store.Save(loaded); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	rt.mu.Lock()
	rt.cfg.Agent.Active = name
	rt.mu.Unlock()

	return nil
}
