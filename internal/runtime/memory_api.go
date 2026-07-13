package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/agentloop"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/config"
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

	// Open the episode index for blended retrieval. The UI rebuilder is wired even
	// when the index is missing so a fresh clone can reconstruct it in-place.
	indexDir := filepath.Join(rt.cfg.Memory.RepoPath, "projects", rt.cfg.Project.ActiveProjectSlug, "index", "_episodes")
	embedClient := embedder.NewClient(
		fmt.Sprintf("http://127.0.0.1:%d", rt.cfg.Embedder.Port),
		httpclient.NewStreaming(),
	)
	epIdx, err := index.Open(indexDir)
	if err != nil {
		slog.Debug("no episode index found, retrieval will use recency only", "dir", indexDir)
	} else {
		rt.assembler = rt.assembler.WithBlendedRetrieval(epIdx, embedClient)
	}
	uiServer.SetRetrievalScorer(&indexScorer{
		indexDir: indexDir,
		emb:      embedClient,
		cfg:      rt.cfg.Prompt,
		idx:      epIdx,
	})
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
	// Wire dedup checker for M6 fact deduplication.
	uiServer.SetDedupChecker(&dedupChecker{
		mem: rt.memReader,
		emb: embedClient,
	})
	uiServer.SetPromotionDedupThreshold(rt.cfg.Prompt.PromotionDedupThreshold)

	asmAdapter := &apiAssemblerAdapter{rt: rt}
	uiServer.SetIndexRebuilder(&indexRebuilder{
		mem:      rt.memReader,
		emb:      embedClient,
		idx:      epIdx,
		indexDir: indexDir,
		repoPath: rt.cfg.Memory.RepoPath,
		slug:     rt.cfg.Project.ActiveProjectSlug,
		gitRepo:  rt.gitRepo,
		onRebuilt: func(idx *index.Index) {
			rt.mu.Lock()
			if rt.assembler != nil {
				rt.assembler = rt.assembler.WithBlendedRetrieval(idx, embedClient)
			}
			rt.mu.Unlock()
		},
	})
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

	// Build the M7 permission evaluator with layered rules:
	// agent defaults → user config → session approvals.
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
	var loopMetrics agentloop.MetricsRecorder
	if metricsStore != nil {
		loopMetrics = metrics.NewRecorder(metricsStore)
	}
	taskAdapter := &taskRunnerAdapter{
		rt:       rt,
		registry: registry,
		asm:      asmAdapter,
		q:        rt.reqQueue,
		evl:      approvals.NewEvaluator(approvals.DefaultLayer(), userLayer),
		metrics:  loopMetrics,
	}
	rt.taskRunner = taskAdapter
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
	rt.taskRunner = nil
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

func (rt *Runtime) getAssembler() *prompt.DiskAssembler {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.assembler
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

// indexScorer implements ui.RetrievalScorer for the active project's episode
// index. It opens lazily so the memory browser can show scores immediately
// after a fresh-clone rebuild creates the index files.
type indexScorer struct {
	mu       sync.Mutex
	indexDir string
	emb      embedder.Client
	cfg      config.PromptConfig
	idx      *index.Index
}

func (s *indexScorer) ScoreEpisodes(ctx context.Context, _, _ string, query string, episodePaths []string) (map[string]ui.RetrievalScore, error) {
	out := make(map[string]ui.RetrievalScore, len(episodePaths))
	for _, p := range episodePaths {
		out[p] = ui.RetrievalScore{}
	}
	idx, err := s.open()
	if err != nil {
		return out, nil
	}

	for _, p := range episodePaths {
		id := episodeID(p)
		score := out[p]
		score.Indexed = idx.Contains(id)
		out[p] = score
	}
	if strings.TrimSpace(query) == "" || s.emb == nil || len(episodePaths) == 0 {
		return out, nil
	}

	vecs, err := s.emb.Embed(ctx, []string{query})
	if err != nil {
		return out, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return out, nil
	}
	results, err := idx.Search(vecs[0], len(episodePaths)*2)
	if err != nil {
		return out, err
	}
	semantic := make(map[string]float64, len(results))
	for _, r := range results {
		if existing, ok := semantic[r.SHA]; !ok || float64(r.Score) > existing {
			semantic[r.SHA] = float64(r.Score)
		}
	}

	oldestFirst := append([]string(nil), episodePaths...)
	sort.Strings(oldestFirst)
	n := float64(len(oldestFirst))
	for i, p := range oldestFirst {
		score := out[p]
		score.Score = s.cfg.SemanticWeight*semantic[episodeID(p)] +
			s.cfg.RecencyWeight*retrievalDecay(len(oldestFirst)-1-i, n)
		score.HasScore = true
		out[p] = score
	}
	return out, nil
}

func (s *indexScorer) open() (*index.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx != nil {
		return s.idx, nil
	}
	idx, err := index.Open(s.indexDir)
	if err != nil {
		return nil, err
	}
	s.idx = idx
	return idx, nil
}

// indexRebuilder implements ui.IndexRebuilder by walking episode files,
// re-embedding any SHA missing from the index, and committing the
// updated manifest and vectors. The operation is idempotent: already-
// indexed episodes are skipped.
type indexRebuilder struct {
	mem       *memory.DirReader
	emb       embedder.Client
	idx       *index.Index
	indexDir  string
	repoPath  string
	slug      string
	gitRepo   *gitw.Repo
	onRebuilt func(*index.Index)
}

func (rb *indexRebuilder) Rebuild(ctx context.Context) error {
	if rb.idx == nil {
		if idx, err := index.Open(rb.indexDir); err == nil {
			rb.idx = idx
		}
	}

	// Walk all episode files under the active project. DirReader.Glob only
	// supports wildcards in the final path segment, so recursive episode lookup
	// needs Walk rather than a projects/<slug>/episodes/*/*.md glob.
	episodesRoot := path.Join("projects", rb.slug, "episodes")
	entries, err := rb.mem.Walk(episodesRoot)
	if err != nil {
		return fmt.Errorf("index rebuild: walk episodes: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Dir || !strings.HasSuffix(e.Path, ".md") {
			continue
		}
		paths = append(paths, e.Path)
	}
	sort.Strings(paths)

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
		if rb.idx != nil && rb.idx.Contains(id) {
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
		chunks := chunkSummary(w.content)
		allChunks = append(allChunks, chunks...)
		chunkCounts[i] = len(chunks)
	}

	vectors, err := rb.emb.Embed(ctx, allChunks)
	if err != nil {
		return fmt.Errorf("index rebuild: embed: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil
	}
	if len(vectors) != len(allChunks) {
		return fmt.Errorf("index rebuild: embed returned %d vectors for %d chunks", len(vectors), len(allChunks))
	}
	dim := len(vectors[0])
	for i, v := range vectors {
		if len(v) != dim {
			return fmt.Errorf("index rebuild: vector %d dimension mismatch: got %d, want %d", i, len(v), dim)
		}
	}
	if rb.idx == nil {
		idx, err := index.Create(rb.indexDir, dim)
		if err != nil {
			return fmt.Errorf("index rebuild: create index %s: %w", rb.indexDir, err)
		}
		rb.idx = idx
	}
	if rb.idx.Dim() != dim {
		return fmt.Errorf("index rebuild: dimension mismatch: index has %d, got %d", rb.idx.Dim(), dim)
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
	if rb.onRebuilt != nil {
		rb.onRebuilt(rb.idx)
	}
	return nil
}

func episodeID(epPath string) string {
	return strings.TrimSuffix(path.Base(epPath), ".md")
}

func retrievalDecay(distanceFromNewest int, n float64) float64 {
	return math.Exp(-float64(distanceFromNewest) / n)
}

// dedupChecker implements ui.DedupChecker by embedding the candidate text and
// existing facts, then comparing cosine similarity.
type dedupChecker struct {
	mem *memory.DirReader
	emb embedder.Client
}

func (dc *dedupChecker) CheckSimilar(ctx context.Context, text string, threshold float64) (bool, string, float64, error) {
	if dc.mem == nil || dc.emb == nil {
		return false, "", 0, nil
	}
	existing, err := dc.mem.Read("global/facts.md")
	if err != nil || len(existing) == 0 {
		return false, "", 0, nil
	}
	// Split existing facts into individual lines — non-empty, trimmed.
	facts := extractFactLines(string(existing))
	if len(facts) == 0 {
		return false, "", 0, nil
	}

	// Embed the candidate + all existing facts in one batch call.
	chunks := make([]string, 0, len(facts)+1)
	chunks = append(chunks, text)
	chunks = append(chunks, facts...)

	vectors, err := dc.emb.Embed(ctx, chunks)
	if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
		return false, "", 0, err
	}
	if len(vectors) != len(chunks) {
		return false, "", 0, fmt.Errorf("embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
	}

	candidate := vectors[0]
	bestScore := 0.0
	bestFact := ""
	for i, v := range vectors[1:] {
		sim := cosineSimilarity(candidate, v)
		if sim > bestScore {
			bestScore = sim
			bestFact = facts[i]
			// Truncate for the flash message.
			if len(bestFact) > 120 {
				bestFact = bestFact[:120] + "..."
			}
		}
	}
	if bestScore >= threshold {
		return true, bestFact, bestScore, nil
	}
	return false, "", bestScore, nil
}

// extractFactLines splits content into non-empty trimmed lines. The facts.md
// file stores one fact per line; leading dashes and bullets are stripped.
func extractFactLines(content string) []string {
	lines := strings.Split(content, "\n")
	facts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-* \t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		facts = append(facts, line)
	}
	return facts
}

// cosineSimilarity returns the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
