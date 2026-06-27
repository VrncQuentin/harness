package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"strings"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/embedder"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/metrics"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/session"
	"github.com/vrnc/harness/internal/tools"
	"github.com/vrnc/harness/internal/ui"
	"github.com/vrnc/harness/pkg/httpclient"
)

// startMemoryAndAPI brings up the memory reader, agent registry, prompt
// assembler, hot-reload watcher, session manager, and API server.
// Caller must hold rt.mu.
//
// metricsStore may be nil; the session manager simply skips metric
// emission in that case.
func (rt *Runtime) startMemoryAndAPI(ctx context.Context, uiServer *ui.Server, metricsStore metrics.Store) {
	uiServer.SetMemoryRepoPath(rt.cfg.Memory.RepoPath)
	if err := memory.ValidateRepo(rt.cfg.Memory.RepoPath); err != nil {
		uiServer.SetAgentRegistry(nil)
		uiServer.SetMemoryStore(nil)
		uiServer.SetSessionStore(nil)
		uiServer.AddStartupError(fmt.Errorf("memory repo: %w", err))
		if rt.cfg.API.Enabled {
			uiServer.AddStartupError(errors.New("api server disabled: memory repo is not valid"))
		}
		return
	}

	rt.memReader = memory.NewDirReader(rt.cfg.Memory.RepoPath)
	rt.agentReg = agent.NewDiskRegistry(rt.memReader, rt.getActiveAgent, rt.setActiveAgent)
	rt.assembler = prompt.NewDiskAssembler(rt.memReader, rt.agentReg, rt.cfg.Prompt).WithProjectSlug(rt.cfg.Project.ActiveProjectSlug)

	// Open the episode index for blended retrieval.
	indexDir := filepath.Join(rt.cfg.Memory.RepoPath, "projects", rt.cfg.Project.ActiveProjectSlug, "index", "_episodes")
	epIdx, err := index.Open(indexDir)
	if err != nil {
		slog.Debug("no episode index found, retrieval will use recency only", "dir", indexDir)
	} else {
		embedClient := embedder.NewClient(
			fmt.Sprintf("http://127.0.0.1:%d", rt.cfg.Embedder.Port),
			httpclient.NewStreaming(),
		)
		rt.assembler = rt.assembler.WithBlendedRetrieval(epIdx, embedClient)
		uiServer.SetRetrievalScorer(&indexScorer{idx: epIdx})
		uiServer.SetIndexRebuilder(&indexRebuilder{
			mem:      rt.memReader,
			emb:      embedClient,
			idx:      epIdx,
			repoPath: rt.cfg.Memory.RepoPath,
			slug:     rt.cfg.Project.ActiveProjectSlug,
			gitRepo:  rt.gitRepo,
		})
	}
	uiServer.SetMemoryStore(rt.memReader)

	hr, err := prompt.NewHotReload(rt.cfg.Memory.RepoPath, rt.cfg.Agent.Active, rt.cfg.Project.ActiveProjectSlug, slog.Default())
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("prompt hot-reload: %w", err))
	} else {
		rt.hotReload = hr
	}

	uiServer.SetAgentRegistry(&uiAgentRegistryAdapter{reg: rt.agentReg, mem: rt.memReader, getProjectSlug: rt.getActiveProjectSlug})

	// Session manager is layered on top of the validated memory repo.
	// A failure to open the git repo surfaces as a startup error and
	// silently disables save/resume so the rest of the harness stays
	// usable.
	sessionMgr, sessionAdapter := rt.buildSessionManager(metricsStore, uiServer)
	rt.setSessionManager(sessionMgr)
	if sessionAdapter != nil {
		uiServer.SetSessionStore(sessionAdapter)
	} else {
		uiServer.SetSessionStore(nil)
	}

	// Wire committer for M6 memory promotion.
	if rt.gitRepo != nil {
		uiServer.SetCommitter(rt.gitRepo)
	}

	asmAdapter := &apiAssemblerAdapter{a: rt.assembler, rt: rt}
	if rt.reqQueue != nil {
		uiServer.SetChatRunner(&chatRunnerAdapter{
			asm: asmAdapter,
			q:   rt.reqQueue,
			mgr: sessionMgr,
		})
	} else {
		uiServer.SetChatRunner(nil)
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
	tools.RegisterBuiltins(registry)
	rt.loopRegistry = registry
	taskAdapter := &taskRunnerAdapter{
		rt:       rt,
		registry: registry,
		asm:      asmAdapter,
		q:        rt.reqQueue,
	}
	uiServer.SetTaskRunner(taskAdapter)
}

// buildSessionManager opens the git repo and constructs a session
// manager pointed at the validated memory paths. Returns nil for both
// values when something fails so the caller silently disables save +
// resume rather than crashing the harness on /chat load.
func (rt *Runtime) buildSessionManager(metricsStore metrics.Store, uiServer *ui.Server) (*session.Manager, *uiSessionStoreAdapter) {
	repoPath := rt.cfg.Memory.RepoPath
	repo, err := gitw.Open(repoPath)
	if err != nil {
		uiServer.AddStartupError(fmt.Errorf("session manager: %w", err))
		return nil, nil
	}
	rt.gitRepo = repo

	infClient := inference.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", rt.cfg.Model.Port),
		httpclient.NewStreaming(),
	)

	var rec session.MetricsRecorder
	if metricsStore != nil {
		rec = metrics.NewRecorder(metricsStore)
	}

	embedClient := embedder.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", rt.cfg.Embedder.Port),
		httpclient.NewStreaming(),
	)

	mgr := session.NewManager(session.ManagerDeps{
		Repo:               repo,
		Writer:             rt.memReader,
		Reader:             rt.memReader,
		Inference:          infClient,
		Metrics:            rec,
		SummarizerPrompt:   rt.summarizerPromptFn(),
		ResolveAbsRepoPath: repoPath,
		AfterSave:          rt.afterSaveEmbed(embedClient, repoPath),
	}, rt.cfg.Project.ActiveProjectSlug)
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

// afterSaveEmbed returns an AfterSaveFunc that embeds the episode summary
// and updates the project's _episodes index.
func (rt *Runtime) afterSaveEmbed(embedClient embedder.Client, repoPath string) session.AfterSaveFunc {
	return func(ctx context.Context, result session.SaveResult) error {
		if embedClient == nil || result.Summary == "" {
			return nil
		}
		// Chunk the summary into paragraphs for embedding.
		chunks := chunkSummary(result.Summary)
		if len(chunks) == 0 {
			return nil
		}

		vectors, err := embedClient.Embed(ctx, chunks)
		if err != nil {
			return fmt.Errorf("embed episode %s: %w", result.ID, err)
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return nil
		}

		// Determine index directory from the episode path.
		// EpisodePath is e.g. "projects/<slug>/episodes/<agent>/<id>.md"
		// Index goes at projects/<slug>/index/_episodes/
		indexDir := episodeIndexDir(repoPath, result.EpisodePath)
		dim := len(vectors[0])

		idx, err := index.Open(indexDir)
		if err != nil {
			idx, err = index.Create(indexDir, dim)
			if err != nil {
				return fmt.Errorf("create index %s: %w", indexDir, err)
			}
		}
		if idx.Dim() != dim {
			return fmt.Errorf("index dimension mismatch: index has %d, got %d", idx.Dim(), dim)
		}

		sha := result.ID
		if err := idx.Add(sha, vectors); err != nil {
			return fmt.Errorf("add to index %s: %w", indexDir, err)
		}

		// Commit index files.
		if rt.gitRepo != nil {
			msg := gitw.BuildMessage(
				map[string]string{"type": "index", "episode_id": result.ID},
				"update episode index",
			)
			relVectors := relIndexPath(result.EpisodePath, "vectors.bin")
			relManifest := relIndexPath(result.EpisodePath, "manifest.json")
			if _, err := rt.gitRepo.Commit(msg, []string{relVectors, relManifest}); err != nil {
				slog.Warn("commit index", "err", err)
			}
		}
		return nil
	}
}

// episodeIndexDir resolves the _episodes index directory from an episode path.
func episodeIndexDir(repoRoot, episodePath string) string {
	// EpisodePath: "projects/<slug>/episodes/<agent>/<id>.md"
	// Index dir:    projects/<slug>/index/_episodes/
	dir := filepath.Dir(filepath.Dir(episodePath)) // strip <agent>/<id>.md
	parts := strings.SplitN(dir, string(filepath.Separator), 3)
	if len(parts) >= 2 && parts[0] == "projects" {
		return filepath.Join(repoRoot, parts[0], parts[1], "index", "_episodes")
	}
	return filepath.Join(repoRoot, dir, "index", "_episodes")
}

func relIndexPath(episodePath, file string) string {
	// Use path (forward slashes) for repo-relative paths as go-git requires.
	// EpisodePath: "projects/<slug>/episodes/<agent>/<id>.md"
	// Index path:  "projects/<slug>/index/_episodes/<file>"
	dir := path.Dir(path.Dir(episodePath))            // "projects/<slug>/episodes" → "projects/<slug>"
	return path.Join(dir, "index", "_episodes", file) // "projects/<slug>/index/_episodes/<file>"
}

func chunkSummary(summary string) []string {
	var chunks []string
	for _, para := range strings.Split(summary, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		chunks = append(chunks, para)
	}
	return chunks
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
	if rt.hotReload != nil {
		if err := rt.hotReload.Close(); err != nil {
			slog.Warn("prompt hot-reload close", "err", err)
		}
		rt.hotReload = nil
	}
	rt.memReader = nil
	rt.agentReg = nil
	rt.assembler = nil
	rt.gitRepo = nil
	rt.setSessionManager(nil)
	uiServer.SetAgentRegistry(nil)
	uiServer.SetMemoryStore(nil)
	uiServer.SetChatRunner(nil)
	uiServer.SetSessionStore(nil)
	uiServer.SetTaskRunner(nil)
	uiServer.SetRetrievalScorer(nil)
	uiServer.SetIndexRebuilder(nil)
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

func (rt *Runtime) setActiveAgent(name string) error {
	rt.mu.Lock()
	store := rt.cfgStore
	hr := rt.hotReload
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

	if hr != nil {
		hr.SetActiveAgent(name)
	}
	return nil
}

// indexScorer implements ui.RetrievalScorer by looking up the episode
// ID in the ANN index manifest. It returns 1.0 when the episode is
// indexed (available for semantic retrieval), -1.0 otherwise so the
// UI can distinguish "not indexed" from "score is zero".
type indexScorer struct {
	idx *index.Index
}

func (s *indexScorer) ScoreEpisode(_ context.Context, episodePath string) (float64, error) {
	if s.idx == nil {
		return -1, nil
	}
	base := path.Base(episodePath)
	id := strings.TrimSuffix(base, ".md")
	if s.idx.Contains(id) {
		return 1.0, nil
	}
	return -1, nil
}

// indexRebuilder implements ui.IndexRebuilder by walking episode files,
// re-embedding any SHA missing from the index, and committing the
// updated manifest and vectors. The operation is idempotent: already-
// indexed episodes are skipped.
type indexRebuilder struct {
	mem      *memory.DirReader
	emb      embedder.Client
	idx      *index.Index
	repoPath string
	slug     string
	gitRepo  *gitw.Repo
}

func (rb *indexRebuilder) Rebuild(ctx context.Context) error {
	// Walk all episode files under the active project.
	pattern := fmt.Sprintf("projects/%s/episodes/*/*.md", rb.slug)
	paths, err := rb.mem.Glob(pattern)
	if err != nil {
		return fmt.Errorf("index rebuild: glob episodes: %w", err)
	}

	if len(paths) == 0 {
		return nil
	}

	// Collect episodes not yet indexed by computing SHA from file path
	// (same ID scheme as afterSaveEmbed: base name without .md).
	type pending struct {
		path    string
		id      string
		content string
	}
	var work []pending
	for _, p := range paths {
		id := strings.TrimSuffix(path.Base(p), ".md")
		if rb.idx.Contains(id) {
			continue
		}
		body, err := rb.mem.Read(p)
		if err != nil {
			slog.Warn("index rebuild: skip unreadable episode", "path", p, "err", err)
			continue
		}
		chunks := chunkSummary(string(body))
		if len(chunks) == 0 {
			continue
		}
		work = append(work, pending{path: p, id: id, content: string(body)})
	}

	if len(work) == 0 {
		return nil
	}

	// Batch embed all pending chunks.
	allChunks := make([]string, 0)
	chunkCounts := make([]int, len(work))
	for i, w := range work {
		chunks := chunkSummary(string(w.content))
		allChunks = append(allChunks, chunks...)
		chunkCounts[i] = len(chunks)
	}

	vectors, err := rb.emb.Embed(ctx, allChunks)
	if err != nil {
		return fmt.Errorf("index rebuild: embed: %w", err)
	}

	// Assign vectors back to each episode and add to index.
	offset := 0
	for i, w := range work {
		n := chunkCounts[i]
		if n == 0 {
			continue
		}
		epVecs := vectors[offset : offset+n]
		offset += n
		if err := rb.idx.Add(w.id, epVecs); err != nil {
			slog.Warn("index rebuild: add episode", "id", w.id, "err", err)
		}
	}

	// Commit the updated index files.
	if rb.gitRepo != nil {
		relVectors := path.Join("projects", rb.slug, "index", "_episodes", "vectors.bin")
		relManifest := path.Join("projects", rb.slug, "index", "_episodes", "manifest.json")
		msg := gitw.BuildMessage(
			map[string]string{"type": "index-rebuild"},
			fmt.Sprintf("rebuild episode index: %d new episodes", len(work)),
		)
		if _, err := rb.gitRepo.Commit(msg, []string{relVectors, relManifest}); err != nil {
			slog.Warn("index rebuild: commit", "err", err)
		}
	}

	slog.Info("index rebuild complete", "new_episodes", len(work))
	return nil
}
