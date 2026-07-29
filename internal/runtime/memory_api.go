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
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store) bool {
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
	if err := memory.ValidateProjectRepo(roots.globalRoot, true); err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("global memory repo: %w", err))
		return false
	}
	if roots.activeSlug != project.GlobalSlug {
		if err := memory.ValidateProjectRepo(roots.activeRoot, false); err != nil {
			uiServer.SetServiceDeps(svcDeps)
			uiServer.AddStartupError(fmt.Errorf("active memory repo: %w", err))
			return false
		}
	}

	// Build the candidate generation entirely in locals.  If any step
	// fails, close everything and leave the current generation untouched.
	globalMem, err := memory.NewDirReader(roots.globalRoot)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("open global memory: %w", err))
		return false
	}
	activeMem, err := memory.NewDirReader(roots.activeRoot)
	if err != nil {
		_ = globalMem.Close()
		uiServer.AddStartupError(fmt.Errorf("open active memory: %w", err))
		return false
	}
	closeCandidates := func() {
		_ = globalMem.Close()
		_ = activeMem.Close()
	}

	agentReg := agent.NewDiskRegistry(globalMem, rt.getActiveAgent, rt.setActiveAgent)
	assembler := prompt.NewProjectDiskAssembler(globalMem, activeMem, agentReg, rt.effectivePromptFor(&rt.cfg)).WithProjectSlug(rt.cfg.Project.ActiveProjectSlug)

	indexDir := memoryops.EpisodeIndexDir(roots.activeRoot)
	episodeIndex, err := memoryops.NewEpisodeIndex(indexDir)
	if err != nil {
		closeCandidates()
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("episode index: %w", err))
		return false
	}
	embedClient := rt.newEmbedderClient()
	assembler = assembler.WithBlendedRetrieval(episodeIndex, embedClient)
	svcDeps.RetrievalScorer = &uiRetrievalScorerAdapter{scorer: &memoryops.EpisodeScorer{
		Embedder: embedClient,
		Config:   rt.cfg.Prompt,
		Index:    episodeIndex,
	}}
	svcDeps.MemoryStore = activeMem
	svcDeps.AgentRegistry = &uiAgentRegistryAdapter{reg: agentReg, globalMem: globalMem, activeMem: activeMem, getProjectSlug: rt.getActiveProjectSlug, setActive: rt.setActiveAgent}

	// Publish the candidate generation and retire the previous one.
	oldGlobal, oldActive := rt.globalMem, rt.activeMem
	rt.globalMem, rt.activeMem = globalMem, activeMem
	rt.agentReg, rt.assembler = agentReg, assembler
	if dr, ok := oldGlobal.(*memory.DirReader); ok {
		_ = dr.Close()
	}
	if dr, ok := oldActive.(*memory.DirReader); ok {
		_ = dr.Close()
	}

	// Session manager is layered on top of the validated memory repo.
	// A failure to open the git repo surfaces as a startup error and
	// silently disables save/resume so the rest of the harness stays
	// usable.
	sessionMgr, sessionAdapter := rt.buildSessionManagerWithClients(metricsStore, uiServer, roots, rt.ensureInferenceClient(), embedClient, episodeIndex)
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
		Mem:       rt.activeMem,
		Embedder:  embedClient,
		Index:     episodeIndex.Current(),
		IndexDir:  indexDir,
		Repo:      rt.gitRepo,
		OnRebuilt: episodeIndex.Replace,
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

type memoryAPISnapshot struct {
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

func (rt *Runtime) restoreMemoryAndAPI(uiServer *ui.Server, snap memoryAPISnapshot) {
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

func (rt *Runtime) buildSessionManagerWithClients(metricsStore metrics.Store, uiServer *ui.Server, roots projectRepoRoots, infClient inference.Client, embedClient embedder.Client, episodeIndexes ...*memoryops.EpisodeIndex) (*session.Manager, *uiSessionStoreAdapter) {
	repoPath := roots.activeRoot
	repo, err := gitw.Open(repoPath)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("session manager: %w", err))
		return nil, nil
	}
	rt.gitRepo = repo

	var episodeIndex *memoryops.EpisodeIndex
	if len(episodeIndexes) > 0 {
		episodeIndex = episodeIndexes[0]
	}
	if episodeIndex == nil {
		episodeIndex, err = memoryops.NewEpisodeIndex(memoryops.EpisodeIndexDir(repoPath))
		if err != nil {
			uiServer.AddStartupError(fmt.Errorf("episode index: %w", err))
			return nil, nil
		}
	}

	var rec session.MetricsRecorder
	if metricsStore != nil {
		rec = metrics.NewRecorder(metricsStore)
	}

	sessionStore, err := memory.NewDirReader(repoPath)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("open session store %s: %w", repoPath, err))
		return nil, nil
	}
	rt.sessionMem = sessionStore
	mgr, err := session.NewManager(session.ManagerDeps{
		Repo:               repo,
		Writer:             sessionStore,
		Reader:             sessionStore,
		Inference:          infClient,
		Metrics:            rec,
		SummarizerPrompt:   rt.summarizerPromptFn(),
		ResolveAbsRepoPath: repoPath,
		AfterSave:          memoryops.AfterSaveEmbed(embedClient, episodeIndex, rt.gitRepo),
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

// quiesceMemoryAndAPI cancels active task loops and flushes live sessions before
// a memory/API rebuild drops the current adapters. Caller must hold rt.mu on
// entry; the method releases it while waiting so session summarization can read
// live config through summarizerPromptFn without deadlocking.
func (rt *Runtime) quiesceMemoryAndAPI(ctx context.Context) {
	tasks := rt.taskRunner
	mgr := rt.SessionManager()
	if tasks == nil && mgr == nil {
		return
	}

	rt.mu.Unlock()
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
	rt.mu.Lock()
}

// stopMemoryAndAPI tears down memory, sessions, task, and API services. Caller
// must hold rt.mu and should call quiesceMemoryAndAPI first when replacing live
// services during reload.
func (rt *Runtime) stopMemoryAndAPI(uiServer *ui.Server) {
	if rt.apiServer != nil {
		rt.apiServer.Stop()
		rt.apiServer = nil
	}
	rt.globalMem = nil
	rt.activeMem = nil
	// sessionMem is not cleared here — it is owned by the session
	// manager and closed on Stop.
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
