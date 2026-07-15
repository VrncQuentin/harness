package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/embedder"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/memoryops"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/project"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/session"
	"github.com/vrnc/harness/internal/tools"
	"github.com/vrnc/harness/internal/ui"
)

// startMemoryAndAPI brings up the memory reader, agent registry, prompt
// assembler, hot-reload watcher, session manager, and API server.
// Caller must hold rt.mu.
//
// metricsStore may be nil; the session manager simply skips metric
// emission in that case.
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store) {
	roots, err := rt.resolveProjectRepoRootsForSlug(rt.cfg.Project.ActiveProjectSlug)
	if err != nil {
		uiServer.SetServiceDeps(ui.ServiceDeps{})
		uiServer.AddStartupError(fmt.Errorf("project memory repos: %w", err))
		if rt.cfg.API.Enabled {
			uiServer.AddStartupError(errors.New("api server disabled: project memory repos are not valid"))
		}
		return
	}

	svcDeps := ui.ServiceDeps{MemoryRepoPath: roots.activeRoot}
	if err := memory.ValidateProjectRepo(roots.globalRoot, true); err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("global memory repo: %w", err))
		return
	}
	if roots.activeSlug != project.GlobalSlug {
		if err := memory.ValidateProjectRepo(roots.activeRoot, false); err != nil {
			uiServer.SetServiceDeps(svcDeps)
			uiServer.AddStartupError(fmt.Errorf("active memory repo: %w", err))
			return
		}
	}

	rt.globalMem = memory.NewDirReader(roots.globalRoot)
	rt.activeMem = memory.NewDirReader(roots.activeRoot)
	rt.agentReg = agent.NewDiskRegistry(rt.globalMem, rt.getActiveAgent, rt.setActiveAgent)
	rt.assembler = prompt.NewProjectDiskAssembler(rt.globalMem, rt.activeMem, rt.agentReg, rt.cfg.Prompt).WithProjectSlug(rt.cfg.Project.ActiveProjectSlug)

	// Open the episode index for blended retrieval. The UI rebuilder is wired even
	// when the index is missing so a fresh clone can reconstruct it in-place.
	indexDir := filepath.Join(roots.activeRoot, "index", "_episodes")
	embedClient := rt.newEmbedderClient()
	epIdx, err := index.Open(indexDir)
	if err != nil {
		slog.Debug("no episode index found, retrieval will use recency only", "dir", indexDir)
	} else {
		rt.assembler = rt.assembler.WithBlendedRetrieval(epIdx, embedClient)
	}
	svcDeps.RetrievalScorer = &memoryops.EpisodeScorer{
		IndexDir: indexDir,
		Embedder: embedClient,
		Config:   rt.cfg.Prompt,
		Index:    epIdx,
	}
	svcDeps.MemoryStore = rt.activeMem
	svcDeps.GlobalMemoryStore = rt.globalMem
	svcDeps.AgentRegistry = &uiAgentRegistryAdapter{reg: rt.agentReg, globalMem: rt.globalMem, activeMem: rt.activeMem, getProjectSlug: rt.getActiveProjectSlug}

	// Session manager is layered on top of the validated memory repo.
	// A failure to open the git repo surfaces as a startup error and
	// silently disables save/resume so the rest of the harness stays
	// usable.
	sessionMgr, sessionAdapter := rt.buildSessionManagerWithClients(metricsStore, uiServer, roots, rt.ensureInferenceClient(), embedClient)
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
		Index:    epIdx,
		IndexDir: indexDir,
		Slug:     rt.cfg.Project.ActiveProjectSlug,
		Repo:     rt.gitRepo,
		OnRebuilt: func(idx *index.Index) {
			rt.mu.Lock()
			if rt.assembler != nil {
				rt.assembler = rt.assembler.WithBlendedRetrieval(idx, embedClient)
			}
			rt.mu.Unlock()
		},
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

	// Wire the M4 task runner (loop engine) with assembler + queue.
	registry := tools.NewRegistry()
	if err := tools.RegisterBuiltins(registry); err != nil {
		uiServer.SetServiceDeps(svcDeps)
		uiServer.AddStartupError(fmt.Errorf("task tools: %w", err))
		return
	}
	rt.loopRegistry = registry

	// Build the M7 permission base layers. Each task engine gets a fresh
	// evaluator so mutable session approval rules stay scoped to that session.
	loopCfg := rt.cfg.Loop
	userLayer := approvals.Layer{Name: "user-config"}
	if !loopCfg.FileWriteEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "file_write", Decision: approvals.Denied, Source: "user: file_write disabled in config",
		})
	}
	if !loopCfg.ShellExecEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "shell_exec", Decision: approvals.Denied, Source: "user: shell_exec disabled in config",
		})
	}
	if !loopCfg.WebSearchEnabled {
		userLayer.Rules = append(userLayer.Rules, approvals.Rule{
			ToolID: "web_search", Decision: approvals.Denied, Source: "user: web_search disabled in config",
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
		approvalLayers: approvalLayers,
		metrics:        loopMetrics,
	}
	rt.taskRunner = taskAdapter
	svcDeps.TaskRunner = taskAdapter
	uiServer.SetServiceDeps(svcDeps)
}

type projectRepoRoots struct {
	globalRoot string
	activeRoot string
	activeSlug string
}

func (rt *Runtime) resolveProjectRepoRootsForSlug(slug string) (projectRepoRoots, error) {
	store, ok := rt.projectStore.(project.Store)
	if !ok || store == nil {
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

// buildSessionManager opens the git repo and constructs a session
// manager pointed at the validated memory paths. Returns nil for both
// values when something fails so the caller silently disables save +
// resume rather than crashing the harness on /chat load.
func (rt *Runtime) buildSessionManager(uiServer *ui.Server, roots projectRepoRoots) (*session.Manager, *uiSessionStoreAdapter) {
	return rt.buildSessionManagerWithClients(nil, uiServer, roots, rt.ensureInferenceClient(), rt.newEmbedderClient())
}

func (rt *Runtime) buildSessionManagerWithClients(metricsStore metrics.Store, uiServer *ui.Server, roots projectRepoRoots, infClient inference.Client, embedClient embedder.Client) (*session.Manager, *uiSessionStoreAdapter) {
	repoPath := roots.activeRoot
	repo, err := gitw.Open(repoPath)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("session manager: %w", err))
		return nil, nil
	}
	rt.gitRepo = repo

	var rec session.MetricsRecorder
	if metricsStore != nil {
		rec = metrics.NewRecorder(metricsStore)
	}

	sessionStore := memory.NewDirReader(repoPath)
	mgr, err := session.NewManager(session.ManagerDeps{
		Repo:               repo,
		Writer:             sessionStore,
		Reader:             sessionStore,
		Inference:          infClient,
		Metrics:            rec,
		SummarizerPrompt:   rt.summarizerPromptFn(),
		ResolveAbsRepoPath: repoPath,
		AfterSave:          memoryops.AfterSaveEmbed(embedClient, repoPath, rt.gitRepo),
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

// stopMemoryAndAPI tears down the M2/M3 services. Caller must hold rt.mu.
//
// The session manager has no goroutines to stop - FlushAll is invoked
// from runtime.Stop() to persist live sessions on shutdown. Resetting
// the field here means a subsequent restart sees a fresh manager.
func (rt *Runtime) stopMemoryAndAPI(uiServer *ui.Server) {
	if rt.apiServer != nil {
		rt.apiServer.Stop()
		rt.apiServer = nil
	}
	rt.globalMem = nil
	rt.activeMem = nil
	rt.agentReg = nil
	rt.assembler = nil
	rt.gitRepo = nil
	rt.setSessionManager(nil)
	uiServer.SetServiceDeps(ui.ServiceDeps{})
	rt.taskRunner = nil
	rt.loopRegistry = nil
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
