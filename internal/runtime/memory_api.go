package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/agentloop"
	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/approvals"
	"github.com/VrncQuentin/harness/internal/config"
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

type memoryCandidate struct {
	globalMem   *memory.DirReader
	activeMem   *memory.DirReader
	agentReg    *agent.DiskRegistry
	assembler   *prompt.DiskAssembler
	sessionMgr  *session.Manager
	taskRunner  *taskRunnerAdapter
	apiServer   *api.Server
	serviceDeps ui.ServiceDeps
	// readers and handles are the candidate's owned resources. The session
	// manager is wired to activeMem directly — there is no separately opened
	// session reader — so every reader in one generation is owned once and
	// closed once.
	readers []*memory.DirReader
	handles []io.Closer
}

// addReader registers an owned memory reader on the candidate.
func (c *memoryCandidate) addReader(r *memory.DirReader) {
	if r != nil {
		c.readers = append(c.readers, r)
	}
}

// addHandle registers an owned closer (episode index, git repository) on the
// candidate.
func (c *memoryCandidate) addHandle(h io.Closer) {
	if h != nil {
		c.handles = append(c.handles, h)
	}
}

func (c *memoryCandidate) close() {
	closed := map[*memory.DirReader]bool{}
	for _, r := range c.readers {
		if !closed[r] {
			closed[r] = true
			_ = r.Close()
		}
	}
	for _, h := range c.handles {
		_ = h.Close()
	}
}

func closeReaders(readers ...memory.Repo) {
	closed := map[*memory.DirReader]bool{}
	for _, r := range readers {
		if dr, ok := r.(*memory.DirReader); ok && !closed[dr] {
			closed[dr] = true
			_ = dr.Close()
		}
	}
}

func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store, candidateCfg *config.Config) bool {
	// Only rebuild the API listener when the port or enabled state changes.
	// On same-port reloads the existing listener stays up and its dynamically
	// resolved adapters pick up the new generation after publication.
	wasRunning := rt.apiServer != nil
	wantRunning := candidateCfg.API.Enabled
	apiPortChanged := wasRunning != wantRunning ||
		(wasRunning && wantRunning && rt.cfg.API.Port != candidateCfg.API.Port)
	buildAPI := candidateCfg.API.Enabled && apiPortChanged

	candidate := rt.buildCandidate(uiServer, metricsStore, candidateCfg, buildAPI)
	if candidate == nil {
		return false
	}

	if candidate.apiServer != nil {
		if err := candidate.apiServer.Start(ctx); err != nil {
			uiServer.AddStartupError(fmt.Errorf("api server: %w", err))
			candidate.close()
			return false
		}
		if rt.apiServer != nil {
			rt.apiServer.Stop()
		}
	} else if apiPortChanged {
		rt.apiServer.Stop()
		rt.apiServer = nil
	}

	oldGlobal := rt.globalMem
	oldActive := rt.activeMem

	rt.globalMem = candidate.globalMem
	rt.activeMem = candidate.activeMem
	rt.agentReg = candidate.agentReg
	rt.assembler = candidate.assembler
	rt.setSessionManager(candidate.sessionMgr)
	rt.taskRunner = candidate.taskRunner
	if candidate.apiServer != nil {
		rt.apiServer = candidate.apiServer
	}

	uiServer.SetServiceDeps(candidate.serviceDeps)

	// Retire the old generation: attach the retired readers and release
	// the publisher lease. When the last in-flight operation releases
	// its lease, the generation's readers are closed.
	if rt.gen != nil {
		rt.gen.readers = []memory.Repo{oldGlobal, oldActive}
		rt.gen.release()
	} else {
		closeReaders(oldGlobal, oldActive)
	}
	rt.gen = &generation{
		assembler:  candidate.assembler,
		sessionMgr: candidate.sessionMgr,
		handles:    candidate.handles,
	}
	rt.gen.acquire()

	return true
}

func (rt *Runtime) buildCandidate(uiServer *ui.Server, metricsStore metrics.Store, cfg *config.Config, buildAPI bool) *memoryCandidate {
	roots, err := rt.resolveProjectRepoRootsForSlug(cfg.Project.ActiveProjectSlug)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("project memory repos: %w", err))
		if cfg.API.Enabled {
			uiServer.AddStartupError(errors.New("api server disabled: project memory repos are not valid"))
		}
		return nil
	}

	if err := memory.ValidateProjectRepo(roots.globalRoot, true); err != nil {
		uiServer.AddStartupError(fmt.Errorf("global memory repo: %w", err))
		return nil
	}
	if roots.activeSlug != project.GlobalSlug {
		if err := memory.ValidateProjectRepo(roots.activeRoot, false); err != nil {
			uiServer.AddStartupError(fmt.Errorf("active memory repo: %w", err))
			return nil
		}
	}

	cand := &memoryCandidate{}
	fail := func(err error) *memoryCandidate {
		cand.close()
		uiServer.AddStartupError(err)
		return nil
	}

	globalMem, err := memory.NewDirReader(roots.globalRoot)
	if err != nil {
		return fail(fmt.Errorf("open global memory: %w", err))
	}
	cand.addReader(globalMem)
	cand.globalMem = globalMem

	// The active reader is the global reader whenever both are configured to
	// the same repository. One generation then owns a single handle for the
	// shared repo — the session manager is wired to this same reader — so
	// there is no second independently opened reader to race or compare.
	activeMem := globalMem
	if roots.activeRoot != roots.globalRoot {
		activeMem, err = memory.NewDirReader(roots.activeRoot)
		if err != nil {
			return fail(fmt.Errorf("open active memory: %w", err))
		}
		cand.addReader(activeMem)
	}
	cand.activeMem = activeMem

	agentReg := agent.NewDiskRegistry(globalMem, rt.getActiveAgent, rt.setActiveAgent)
	assembler := prompt.NewProjectDiskAssembler(globalMem, activeMem, agentReg, rt.effectivePromptFor(cfg)).WithProjectSlug(cfg.Project.ActiveProjectSlug)

	indexDir := memoryops.EpisodeIndexDir(roots.activeRoot)
	if err := activeMem.MkdirAll("index/_episodes"); err != nil {
		return fail(fmt.Errorf("episode index: mkdir: %w", err))
	}
	indexAnchor, err := activeMem.SubAnchor("index/_episodes")
	if err != nil {
		return fail(fmt.Errorf("episode index: %w", err))
	}
	// The episode index serializes on the repository-wide mutation coordinator,
	// so its identity is the repository's verified identity, not the index
	// directory's.
	episodeIndex, err := memoryops.NewEpisodeIndex(indexAnchor, indexDir, activeMem.Identity())
	if err != nil {
		_ = indexAnchor.Close()
		return fail(fmt.Errorf("episode index: %w", err))
	}
	cand.addHandle(episodeIndex)
	embedClient := rt.newEmbedderClientFor(cfg)
	assembler = assembler.WithBlendedRetrieval(episodeIndex, embedClient)

	infClient := rt.newInferenceClientFor(cfg)
	gitRepo, sessionMgr, sessionAdapter, err := rt.buildSessionManagerWithClients(metricsStore, roots, infClient, embedClient, episodeIndex, activeMem, cfg.Project.ActiveProjectSlug)
	cand.addHandle(gitRepo)
	if err != nil {
		return fail(err)
	}

	// The active memory reader — which serves prompt, session, and index I/O —
	// and the git repository must be one physical repository. They are
	// compared by their retained pinned boundaries (os.SameFile), never by
	// pathid identity, so a directory replaced at the same pathname between
	// the two opens fails closed rather than combining a reader and a git
	// handle rooted in different repositories.
	if same, err := activeMem.SameRepo(gitRepo); err != nil || !same {
		return fail(fmt.Errorf("session manager: memory reader and git repository identify different directories: %v", err))
	}

	asmAdapter := &apiAssemblerAdapter{rt: rt}

	loopCfg := cfg.Loop
	userLayer := approvals.Layer{Name: "user-config"}
	if !loopCfg.EditEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "edit", Decision: approvals.Denied, Source: "user: edit disabled in config"})
	}
	if !loopCfg.ExecEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "exec", Decision: approvals.Denied, Source: "user: exec disabled in config"})
	}
	if !loopCfg.GoTestEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "go_test", Decision: approvals.Denied, Source: "user: go_test disabled in config"})
	}
	if !loopCfg.GoLintEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "go_lint", Decision: approvals.Denied, Source: "user: go_lint disabled in config"})
	}
	if !loopCfg.GitCommitEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "git_commit", Decision: approvals.Denied, Source: "user: git_commit disabled in config"})
	}
	if !loopCfg.GitBranchEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "git_branch", Decision: approvals.Denied, Source: "user: git_branch disabled in config"})
	}
	if !loopCfg.GitCheckoutEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "git_checkout", Decision: approvals.Denied, Source: "user: git_checkout disabled in config"})
	}
	if !loopCfg.WebSearchEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "web_search", Decision: approvals.Denied, Source: "user: web_search disabled in config"})
	}
	if !loopCfg.MemoryQueryEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "memory_query", Decision: approvals.Denied, Source: "user: memory_query disabled in config"})
	}
	if !loopCfg.GitPushEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "git_push", Decision: approvals.Denied, Source: "user: git_push disabled in config"})
	}
	if !loopCfg.GHPRCreateEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "gh_pr_create", Decision: approvals.Denied, Source: "user: gh_pr_create disabled in config"})
	}
	if !loopCfg.GHPRMergeEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "gh_pr_merge", Decision: approvals.Denied, Source: "user: gh_pr_merge disabled in config"})
	}
	if !loopCfg.GHPRWaitEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{ToolID: "gh_pr_wait", Decision: approvals.Denied, Source: "user: gh_pr_wait disabled in config"})
	}
	approvalLayers := []approvals.Layer{approvals.DefaultLayer(), userLayer}

	var gov agentloop.Governor
	var tooloutDir string
	if harnessHome, err := home.Default(); err == nil {
		cacheDir := filepath.Join(harnessHome, "cache")
		tooloutDir = governor.TooloutDir(cacheDir)
		if parsers, err := parser.NewRegistry(parser.NewGoFrontEnd()); err == nil {
			gov = governor.New(parsers, cacheDir)
		}
	}

	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry); err != nil {
		return fail(fmt.Errorf("task tools: %w", err))
	}

	var loopMetrics agentloop.MetricsRecorder
	if metricsStore != nil {
		loopMetrics = metrics.NewRecorder(metricsStore)
	}
	taskAdapter := &taskRunnerAdapter{
		rt:             rt,
		registry:       registry,
		asm:            asmAdapter,
		q:              rt.reqQueue,
		memScorer:      &memoryops.EpisodeScorer{Embedder: embedClient, Config: cfg.Prompt, Index: episodeIndex},
		approvalLayers: approvalLayers,
		metrics:        loopMetrics,
		gov:            gov,
		tooloutDir:     tooloutDir,
	}

	var apiSrv *api.Server
	if buildAPI {
		apiSrv = api.NewServer(cfg.API.Port, asmAdapter, rt.reqQueue, nil)
		apiSrv.WithGenLease(rt.AcquireRequestGeneration)
	}

	svcDeps := ui.ServiceDeps{MemoryRepoPath: roots.activeRoot}
	svcDeps.RetrievalScorer = &uiRetrievalScorerAdapter{scorer: &memoryops.EpisodeScorer{
		Embedder: embedClient,
		Config:   cfg.Prompt,
		Index:    episodeIndex,
	}}
	svcDeps.MemoryStore = activeMem
	svcDeps.AgentRegistry = &uiAgentRegistryAdapter{reg: agentReg, globalMem: globalMem, activeMem: activeMem, getProjectSlug: rt.getActiveProjectSlug, setActive: rt.setActiveAgent}
	if sessionAdapter != nil {
		svcDeps.SessionStore = sessionAdapter
	}
	if gitRepo != nil {
		svcDeps.Committer = gitRepo
	}
	svcDeps.Dedup = &memoryops.DedupChecker{
		Mem:      activeMem,
		Embedder: embedClient,
	}
	svcDeps.PromotionDedupThreshold = cfg.Prompt.PromotionDedupThreshold
	svcDeps.IndexRebuilder = &memoryops.EpisodeRebuilder{
		Mem:       activeMem,
		Embedder:  embedClient,
		Index:     episodeIndex.Current(),
		IndexDir:  indexDir,
		Repo:      gitRepo,
		OnRebuilt: episodeIndex.Replace,
		EI:        episodeIndex,
	}
	if rt.reqQueue != nil {
		svcDeps.ChatRunner = &chatRunnerAdapter{
			asm: asmAdapter,
			q:   rt.reqQueue,
			mgr: sessionMgr,
		}
	}
	svcDeps.TaskRunner = taskAdapter

	cand.agentReg = agentReg
	cand.assembler = assembler
	cand.sessionMgr = sessionMgr
	cand.taskRunner = taskAdapter
	cand.apiServer = apiSrv
	cand.serviceDeps = svcDeps
	return cand
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

func (rt *Runtime) buildSessionManagerWithClients(metricsStore metrics.Store, roots projectRepoRoots, infClient inference.Client, embedClient embedder.Client, episodeIndex *memoryops.EpisodeIndex, sessionReader *memory.DirReader, projectSlug string) (*gitw.Repo, *session.Manager, *uiSessionStoreAdapter, error) {
	repoPath := roots.activeRoot
	if rt.beforeGitOpen != nil {
		rt.beforeGitOpen()
	}
	repo, err := gitw.Open(repoPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("session manager: %w", err)
	}

	var rec session.MetricsRecorder
	if metricsStore != nil {
		rec = metrics.NewRecorder(metricsStore)
	}

	// The session manager reads and writes the same generation-owned reader
	// the active memory and episode index use, so there is no separately
	// opened session handle to race against the git boundary.
	mgr, err := session.NewManager(session.ManagerDeps{
		Repo:               repo,
		Writer:             sessionReader,
		Reader:             sessionReader,
		Inference:          infClient,
		Metrics:            rec,
		SummarizerPrompt:   rt.summarizerPromptFn(),
		ResolveAbsRepoPath: repoPath,
		AfterSave:          memoryops.AfterSaveEmbed(embedClient, episodeIndex, repo),
	}, projectSlug)
	if err != nil {
		return repo, nil, nil, fmt.Errorf("session manager: %w", err)
	}
	adapter := &uiSessionStoreAdapter{mgr: mgr, getActive: rt.getActiveAgent}
	return repo, mgr, adapter, nil
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

func (rt *Runtime) memoryAPIUnavailable() bool {
	return rt.globalMem == nil ||
		rt.activeMem == nil ||
		rt.agentReg == nil ||
		rt.assembler == nil ||
		rt.taskRunner == nil ||
		rt.SessionManager() == nil ||
		(rt.cfg.API.Enabled && rt.apiServer == nil)
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
