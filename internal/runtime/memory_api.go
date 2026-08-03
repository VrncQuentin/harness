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
	if c.apiServer != nil {
		c.apiServer.Stop()
	}
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

// applyTx is a locally owned apply transaction. It owns a prepared memory
// candidate (including its API server, if any) that commit will install. A
// transaction that is not committed must be closed as one object: close stops
// the API server it started and closes every candidate handle. commitApply
// transfers ownership of the candidate's resources to the runtime, after which
// the caller must not call close.
type applyTx struct {
	candidate *memoryCandidate
}

func (tx *applyTx) close() {
	if tx == nil || tx.candidate == nil {
		return
	}
	tx.candidate.close()
	tx.candidate = nil
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

// startMemoryAndAPI builds, publishes, and records the applied state for the
// initial generation of a freshly started runtime (the Start path and the
// first-start branch of ApplyConfig). The running model is the preferred model
// because nothing is running yet. Caller must hold rt.mu on production paths.
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store, candidateCfg *config.Config) bool {
	runningModel := rt.effectiveModelFor(candidateCfg)
	apiPortChanged := rt.apiPortChangeFromLive(candidateCfg)
	buildAPI := candidateCfg.API.Enabled && apiPortChanged

	tx := rt.prepareApply(ctx, uiServer, metricsStore, candidateCfg, runningModel, buildAPI)
	if tx == nil {
		return false
	}

	rt.installGeneration(tx.candidate)
	rt.transferAPIServer(tx.candidate, apiPortChanged)
	rt.activateAPIServer(tx.candidate.apiServer)

	applied := newAppliedState(candidateCfg, runningModel, runningModel)
	rt.applied = &applied
	return true
}

// activateAPIServer starts serving a candidate's bound API listener. It is
// called only when the candidate commits, so no request can reach a prepared
// server before the generation it was prepared for is installed.
func (rt *Runtime) activateAPIServer(srv *api.Server) {
	if srv == nil {
		return
	}
	srv.Serve()
}

// apiPortChangeFromLive reports whether the API listener must be rebuilt when
// applying candidateCfg, comparing against the currently installed API server
// and committed config (used by the first-start path where no applied state
// exists yet).
func (rt *Runtime) apiPortChangeFromLive(candidateCfg *config.Config) bool {
	wasRunning := rt.apiServer != nil
	wantRunning := candidateCfg.API.Enabled
	if wasRunning != wantRunning {
		return true
	}
	return wasRunning && rt.cfg.API.Port != candidateCfg.API.Port
}

// prepareApply builds a candidate for cfg and binds its API listener when the
// port/enabled state changed. The transaction is locally owned and unpublished:
// nothing in the runtime is mutated, no process is touched, and the bound
// listener accepts no requests until commitApply installs the candidate and
// activates it (Serve). A failed preparation is discarded wholesale via
// tx.close; the installed generation and recorded applied state are untouched.
func (rt *Runtime) prepareApply(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store, cfg *config.Config, runningModel config.ModelConfig, buildAPI bool) *applyTx {
	candidate := rt.buildCandidate(uiServer, metricsStore, cfg, buildAPI, runningModel)
	if candidate == nil {
		return nil
	}
	tx := &applyTx{candidate: candidate}
	if candidate.apiServer != nil {
		if err := candidate.apiServer.Bind(ctx); err != nil {
			uiServer.AddStartupError(fmt.Errorf("api server: %w", err))
			tx.close()
			return nil
		}
	}
	return tx
}

// installGeneration swaps the live generation for the candidate's and retires
// the old publisher lease under the same lock acquisition uses, so a UI
// handler cannot select an old snapshot after its generation was retired. Old
// readers close only after the last acquired snapshot on the old generation is
// released. Caller must hold rt.mu.
func (rt *Runtime) installGeneration(c *memoryCandidate) {
	oldGlobal := rt.globalMem
	oldActive := rt.activeMem

	// Bind the candidate's complete UI snapshot to its generation before
	// anything observes it, and take the publisher lease up front.
	newGen := &generation{
		assembler:  c.assembler,
		sessionMgr: c.sessionMgr,
		handles:    c.handles,
		uiSnap:     c.serviceDeps,
	}
	newGen.acquire()

	rt.globalMem = c.globalMem
	rt.activeMem = c.activeMem
	rt.agentReg = c.agentReg
	rt.assembler = c.assembler
	rt.setSessionManager(c.sessionMgr)
	rt.taskRunner = c.taskRunner

	oldGen := rt.gen
	rt.gen = newGen
	if oldGen != nil {
		oldGen.readers = []memory.Repo{oldGlobal, oldActive}
		oldGen.release()
	} else {
		closeReaders(oldGlobal, oldActive)
	}
}

// transferAPIServer swaps the live API server for a prepared one under the
// timeout ownership protocol. The previous server is handed to the
// pending-retirement list; its Stop runs outside rt.mu (see drainPendingRetired)
// because shutdown can wait on active connections. Caller must hold rt.mu.
func (rt *Runtime) transferAPIServer(c *memoryCandidate, apiPortChanged bool) {
	if c.apiServer != nil {
		if rt.apiServer != nil {
			rt.pendingRetiredAPI = append(rt.pendingRetiredAPI, rt.apiServer)
		}
		rt.apiServer = c.apiServer
	} else if apiPortChanged && rt.apiServer != nil {
		rt.pendingRetiredAPI = append(rt.pendingRetiredAPI, rt.apiServer)
		rt.apiServer = nil
	}
}

// drainPendingRetired stops the API servers the current commit retired. A
// server whose Stop does not confirm termination within the shutdown timeout
// is moved to rt.retiredAPI so the runtime retains ownership until a later
// Stop confirms termination. It must be called without rt.mu held because Stop
// waits.
func (rt *Runtime) drainPendingRetired() {
	rt.mu.Lock()
	pending := append([]*api.Server(nil), rt.pendingRetiredAPI...)
	rt.pendingRetiredAPI = nil
	rt.mu.Unlock()

	kept := make([]*api.Server, 0, len(pending))
	for _, srv := range pending {
		if !rt.stopAPIServer(srv) {
			kept = append(kept, srv)
		}
	}
	if len(kept) == 0 {
		return
	}
	rt.mu.Lock()
	rt.retiredAPI = append(rt.retiredAPI, kept...)
	rt.mu.Unlock()
}

// drainRetiredAPI is the terminal drain, used by Stop, Shutdown, and tests:
// it stops anything still pending from the last commit and re-attempts servers
// whose earlier shutdown timed out. A server that still refuses to terminate
// keeps its slot; ownership is retained. It returns true when every server
// confirmed termination. It must be called without rt.mu held because Stop
// waits.
func (rt *Runtime) drainRetiredAPI() bool {
	rt.mu.Lock()
	pending := append([]*api.Server(nil), rt.pendingRetiredAPI...)
	rt.pendingRetiredAPI = nil
	pending = append(pending, rt.retiredAPI...)
	rt.retiredAPI = nil
	rt.mu.Unlock()

	allConfirmed := true
	kept := make([]*api.Server, 0, len(pending))
	for _, srv := range pending {
		if !rt.stopAPIServer(srv) {
			allConfirmed = false
			kept = append(kept, srv)
		}
	}
	rt.mu.Lock()
	rt.retiredAPI = kept
	rt.mu.Unlock()
	return allConfirmed
}

// commitApply installs a prepared apply under rt.mu: the generation and applied
// state swap atomically, process reconfigurations are issued from the new
// applied state (never re-derived from the stores), and the previous API
// server is retired under the timeout ownership protocol. tx is nil when no
// memory/API rebuild is needed. The commit is structured to be infallible so
// the installed applied state is always coherent with the live processes.
func (rt *Runtime) commitApply(tx *applyTx, newApplied *appliedState, oldApplied *appliedState, modelChanged, embedderChanged, endpointChanged, apiPortChanged bool, oldCfg config.Config, uiServer *ui.Server) ui.ApplyResult {
	var result ui.ApplyResult

	if tx != nil {
		rt.installGeneration(tx.candidate)
		rt.transferAPIServer(tx.candidate, apiPortChanged)
		rt.activateAPIServer(tx.candidate.apiServer)
		result.LiveApplied = true
	}

	if modelChanged && rt.llamaMgr != nil {
		slog.Info("reconfiguring llama-server", "old_port", oldApplied.runningModel.Port, "new_port", newApplied.runningModel.Port)
		rt.llamaMgr.Reconfigure(func() (string, []string) { return llamaArgsForModel(newApplied.runningModel) }, llamaHealthURL(newApplied.runningModel))
		result.LiveApplied = true
	}
	if embedderChanged && rt.embedMgr != nil {
		slog.Info("reconfiguring embedder", "old_port", oldApplied.runningEmbedder.Port, "new_port", newApplied.runningEmbedder.Port)
		rt.embedMgr.Reconfigure(func() (string, []string) { return embedderArgsForConfig(newApplied.runningEmbedder) }, embedderHealthURL(newApplied.runningEmbedder))
		result.LiveApplied = true
	}
	if endpointChanged && rt.reqQueue != nil {
		client := rt.newInferenceClientForPort(newApplied.runningModel.Port)
		rt.inferClient = client
		rt.reqQueue.SetClient(client)
	}

	rt.cfg = newApplied.cfg
	rt.applied = newApplied
	rt.refreshProjectDirectoryWarnings(uiServer)
	rt.finishResult(&result, oldCfg, &newApplied.cfg)
	return result
}

func (rt *Runtime) buildCandidate(uiServer *ui.Server, metricsStore metrics.Store, cfg *config.Config, buildAPI bool, runningModel config.ModelConfig) *memoryCandidate {
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
	// The prompt context ceiling and the inference client track the running
	// model, not the preferred one: under llama_on_switch=keep the running
	// model (and its ctx/port) may lag the active project's preference, and
	// the harness must keep talking to where llama-server actually runs.
	assembler := prompt.NewProjectDiskAssembler(globalMem, activeMem, agentReg, promptConfigFor(cfg, runningModel)).WithProjectSlug(cfg.Project.ActiveProjectSlug)

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

	infClient := rt.newInferenceClientForPort(runningModel.Port)
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

	// The UI snapshot's runners and registry adapters are bound to concrete
	// candidate-generation resources and config, never to adapters that
	// reacquire the live generation at execution time. The API server alone
	// keeps the dynamic asmAdapter, because API requests legitimately use the
	// current generation.
	//
	// The snapshot's static assembler deliberately carries no active agent:
	// /agents/active switches the selection without a generation rebuild, so
	// the active agent is resolved per acquisition in AcquireUISnapshot
	// (ServiceDeps.ActiveAgent) and the chat/task handlers pass it explicitly.
	snapshotAsm := &staticAssembler{asm: assembler}

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
		asm:            snapshotAsm,
		registry:       registry,
		q:              rt.reqQueue,
		sessionMgr:     sessionMgr,
		slug:           cfg.Project.ActiveProjectSlug,
		loopCfg:        loopCfg,
		mem:            activeMem,
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
	svcDeps.AgentRegistry = &uiAgentRegistryAdapter{reg: agentReg, globalMem: globalMem, activeMem: activeMem, slug: cfg.Project.ActiveProjectSlug, setActive: rt.setActiveAgent}
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
			asm: snapshotAsm,
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

	// The session manager reads, writes, and appends through the same
	// generation-owned reader the active memory and episode index use, so
	// there is no separately opened session handle to race against the git
	// boundary. The sessions.jsonl append goes through the same pinned root,
	// not an absolute pathname.
	mgr, err := session.NewManager(session.ManagerDeps{
		Repo:             repo,
		Writer:           sessionReader,
		Reader:           sessionReader,
		Appender:         sessionReader,
		Inference:        infClient,
		Metrics:          rec,
		SummarizerPrompt: rt.summarizerPromptFn(),
		AfterSave:        memoryops.AfterSaveEmbed(embedClient, episodeIndex, repo),
	}, projectSlug)
	if err != nil {
		return repo, nil, nil, fmt.Errorf("session manager: %w", err)
	}
	adapter := &uiSessionStoreAdapter{mgr: mgr}
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

// sessionManager exposes the live session manager for in-package use and
// tests. Returns nil when the repo has not been validated yet.
func (rt *Runtime) sessionManager() *session.Manager {
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
	mgr := rt.sessionManager()
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
		rt.sessionManager() == nil ||
		(rt.cfg.API.Enabled && rt.apiServer == nil)
}

func (rt *Runtime) getActiveAgent() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg.Agent.Active
}

func (rt *Runtime) setActiveAgent(name string) error {
	// The active-agent write is an out-of-band mutation to the config store and
	// live state, so it participates in the same apply transaction as
	// ApplyConfig: an apply that has already loaded an agent and is preparing
	// must not be overwritten by this save, and vice versa. The lock order is
	// applyMu then rt.mu, matching ApplyConfig.
	if rt.beforeApplyMu != nil {
		rt.beforeApplyMu()
	}
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()

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
	// The active agent is the one out-of-band mutation to rt.cfg (it happens
	// through /agents/active without a config apply), so the recorded applied
	// config must follow it or the next apply would compare against a stale
	// selection and rebuild unnecessarily.
	if rt.applied != nil {
		rt.applied.cfg.Agent.Active = name
	}
	rt.mu.Unlock()

	return nil
}
