package ui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
)

// maxMemoryFileBytes caps the size of a file the editor will accept on
// save. Memory files are hand-authored markdown - anything larger is
// almost certainly a paste accident or a wrong path. Reject early so
// the user gets a clear error instead of a silent truncation later.
const maxMemoryFileBytes = 1 << 20 // 1 MiB

// agentsDirName is the top-level directory under the memory repo that
// holds per-agent files (persona, rules, notes, episodes). The /memory
// page treats it specially when totalling tokens: at most one agent's
// content lands in any single prompt, so the displayed total uses the
// largest single agent rather than the sum across agents.
const agentsDirName = "agents"

// episodesRoot is the project-scoped directory under the memory repo
// where session episodes live. The browser lists files under
// <episodesRoot>/<agent>/, sorted newest-first by ISO timestamp.
// episodeFileSuffix is the on-disk extension for an episode file.
// Anything else under the agent directory is ignored by the browser
// because only markdown is committed by the session writer.
const episodeFileSuffix = ".md"

// RetrievalScore is the memory browser's retrieval metadata for one episode.
type RetrievalScore struct {
	Indexed  bool
	Score    float64
	HasScore bool
}

// RetrievalScorer returns index status and, when a query is supplied, the
// blended retrieval score for a batch of episode paths.
type RetrievalScorer interface {
	ScoreEpisodes(ctx context.Context, projectSlug, agent, query string, episodePaths []string) (map[string]RetrievalScore, error)
}

// IndexRebuilder triggers an idempotent index rebuild for one tree.
// The memory browser calls this when the user invokes "Rebuild Index".
// Implementations walk episodes (or directory trees), embed any SHA
// missing from the index, and commit the updated manifest.
type IndexRebuilder interface {
	Rebuild(ctx context.Context) error
}

// MemoryStore is the surface the /memory page needs from the memory
// repo. Implemented by *memory.DirReader; broken out so tests can stub
// the FS without spinning up a real DirReader.
type MemoryStore interface {
	Walk(relPath string) ([]memory.Entry, error)
	Read(relPath string) ([]byte, error)
	WriteFile(relPath string, data []byte) error
}

// editableGlobalFiles enumerates the files the UI lets the user create
// or edit inline. Anything outside this set (agent personas, episodes,
// runtime artifacts) is read-only here so risky edits go through git
// rather than a textarea.
var editableGlobalFiles = []editableFile{
	{Path: "global/rules.md", Desc: "Always-on base prompt"},
	{Path: "global/user.md", Desc: "Hand-authored facts about the user"},
	{Path: "global/facts.md", Desc: "Promoted cross-agent facts"},
}

type editableFile struct {
	Path string
	Desc string
}

// editableDesc returns the description for an editable file, or an
// empty string when path is not editable. Lookup is linear because the
// list is tiny.
func editableDesc(p string) (string, bool) {
	for _, f := range editableGlobalFiles {
		if f.Path == p {
			return f.Desc, true
		}
	}
	return "", false
}

// SetMemoryStore wires the store used by the /memory page. Pass nil to
// detach (e.g. when memory.repo_path is cleared in /config); the page
// then renders the not-configured CTA instead of a blank tree.
func (s *Server) SetMemoryStore(store MemoryStore) {
	s.memStoreMu.Lock()
	s.memStore = store
	s.memStoreMu.Unlock()
}

func (s *Server) memoryStore() MemoryStore {
	s.memStoreMu.RLock()
	defer s.memStoreMu.RUnlock()
	return s.memStore
}

// SetRetrievalScorer wires the scorer used by the memory episode view.
// Pass nil to detach; the page then hides the score column.
func (s *Server) SetRetrievalScorer(scorer RetrievalScorer) {
	s.scorerMu.Lock()
	s.scorerData = scorer
	s.scorerMu.Unlock()
}

func (s *Server) retrievalScorer() RetrievalScorer {
	s.scorerMu.RLock()
	defer s.scorerMu.RUnlock()
	return s.scorerData
}

// SetIndexRebuilder wires the index rebuild handler. Pass nil to
// detach; the memory page hides the rebuild button.
func (s *Server) SetIndexRebuilder(rb IndexRebuilder) {
	s.rebuilderMu.Lock()
	s.rebuilderData = rb
	s.rebuilderMu.Unlock()
}

func (s *Server) indexRebuilder() IndexRebuilder {
	s.rebuilderMu.RLock()
	defer s.rebuilderMu.RUnlock()
	return s.rebuilderData
}

// memoryView is the template context for /memory.
type memoryView struct {
	basePage
	Configured      bool
	RepoPath        string
	AgentsPath      string // absolute path to <RepoPath>/agents (empty when RepoPath is unset)
	Tree            []*memoryTreeNode
	TotalTokens     int
	LoadErr         string
	SavedPath       string // flash: file just saved
	EpisodesByAgent []agentEpisodeCount
	EpisodesLoadErr string
	CanRebuild      bool
	// AgentNames is the list of agent names for the append-note dropdown.
	AgentNames []string
	// Promoted shows a success flash after promoting a fact.
	Promoted bool
	// NotedAgent shows the agent name after successfully appending a note.
	NotedAgent string
	// DedupBlocked is true when a fact promotion was blocked by dedup.
	DedupBlocked bool
	// DedupSimilar is the closest existing fact text when dedup blocked.
	DedupSimilar string
	// DedupScore is the cosine similarity score when dedup blocked.
	DedupScore float64
}

// agentEpisodeCount counts how many .md episodes live under an agent's
// directory in projects/global/episodes/. The /memory page renders one
// row per agent linking to /memory/episodes?agent=<Name>.
type agentEpisodeCount struct {
	Name  string
	Count int
}

// memoryEpisodesView is the template context for /memory/episodes.
type memoryEpisodesView struct {
	basePage
	Agent      string
	AgentNames []string
	Query      string
	Episodes   []episodeRow
}

// episodeRow is one row in the per-agent episode list. Path is the full
// repo-relative path the view handler expects in its `path` query
// parameter; Name is what gets displayed in the table.
type episodeRow struct {
	Name     string
	Path     string
	Tokens   int
	Indexed  bool
	Score    float64
	HasScore bool
}

// memoryEpisodeView is the template context for
// /memory/episodes/view. Content is rendered raw inside a <pre> block
// so the user sees the markdown the session writer committed without
// the harness pulling in a markdown renderer.
type memoryEpisodeView struct {
	basePage
	Agent    string
	Name     string
	Path     string
	Content  string
	Tokens   int
	Query    string
	Indexed  bool
	Score    float64
	HasScore bool
}

// memoryTreeNode is one node in the rendered tree. Children is a slice
// of pointers so directory totals can be filled in after the tree is
// built without copying every node.
type memoryTreeNode struct {
	Name     string
	Path     string
	Dir      bool
	Missing  bool
	Editable bool
	Tokens   int
	// Content is the file body, captured during the walk so the tree
	// page can inline it under an expandable row without a second read.
	// Empty for directories and for missing virtual nodes.
	Content  string
	Children []*memoryTreeNode
}

// memoryEditView is the template context for /memory/edit.
type memoryEditView struct {
	basePage
	Path    string
	Desc    string
	Content string
	Tokens  int
	IsNew   bool
	SaveErr string
}

// handleMemory renders the /memory tree (GET only).
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/memory" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := memoryView{basePage: s.newBasePage("memory")}
	store := s.memoryStore()
	if store != nil {
		data.Configured = true
		data.RepoPath = s.getMemoryRepoPath()
		if data.RepoPath != "" {
			data.AgentsPath = filepath.Join(data.RepoPath, agentsDirName)
		}
		tree, total, err := buildMemoryTree(store)
		if err != nil {
			data.LoadErr = err.Error()
		}
		data.Tree = tree
		data.TotalTokens = total

		counts, err := countEpisodesByAgent(store, s.activeProjectSlug())
		if err != nil {
			data.EpisodesLoadErr = err.Error()
		}
		data.EpisodesByAgent = counts
	}
	data.CanRebuild = s.indexRebuilder() != nil
	if saved := strings.TrimSpace(r.URL.Query().Get("saved")); saved != "" {
		data.SavedPath = saved
	}
	q := r.URL.Query()
	data.Promoted = q.Get("promoted") == "1"
	data.NotedAgent = strings.TrimSpace(q.Get("agent"))
	if q.Get("noted") != "1" {
		data.NotedAgent = ""
	}
	data.DedupBlocked = q.Get("dedup") == "1"
	data.DedupSimilar = strings.TrimSpace(q.Get("similar"))
	if s := strings.TrimSpace(q.Get("score")); s != "" {
		fmt.Sscanf(s, "%f", &data.DedupScore)
	}
	if reg := s.agentRegistry(); reg != nil {
		if list, err := reg.List(); err == nil {
			for _, a := range list {
				data.AgentNames = append(data.AgentNames, a.Name)
			}
		}
	}
	s.renderMemory(w, data)
}

// handleMemoryEpisodes renders the per-agent episode list. It reads
// projects/global/episodes/<agent>/ via the memory store, filters for
// .md files, sorts newest-first by filename (ISO timestamps sort
// lexicographically, so a reverse string sort is the cheapest way to
// approximate "newest first" without parsing each name), and renders a
// table linking each row to /memory/episodes/view.
func (s *Server) handleMemoryEpisodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not configured", http.StatusServiceUnavailable)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if !validAgentName(agent) {
		http.Error(w, "invalid agent name", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	scorer := s.retrievalScorer()
	rows, err := listAgentEpisodes(r.Context(), store, s.activeProjectSlug(), agent, query, scorer)
	if err != nil {
		http.Error(w, "could not list episodes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := memoryEpisodesView{
		basePage: s.newBasePage("memory"),
		Agent:    agent,
		Query:    query,
		Episodes: rows,
	}
	if reg := s.agentRegistry(); reg != nil {
		if list, err := reg.List(); err == nil {
			for _, a := range list {
				data.AgentNames = append(data.AgentNames, a.Name)
			}
		}
	}
	s.renderMemoryEpisodes(w, data)
}

// handleMemoryEpisodeView renders one episode's content. The path query
// parameter must be a repo-relative path under projects/global/episodes/
// that ends in .md - anything else is rejected to keep this endpoint
// from doubling as a generic file viewer.
func (s *Server) handleMemoryEpisodeView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not configured", http.StatusServiceUnavailable)
		return
	}

	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	// Defence in depth: the underlying Reader's checkRel rejects
	// traversal too, but a 400 here is clearer than the generic
	// downstream error and keeps this endpoint from looking like a
	// generic file reader.
	if strings.Contains(p, "..") {
		http.Error(w, "path must not contain ..", http.StatusBadRequest)
		return
	}
	episodesRoot := episodesRootForSlug(s.activeProjectSlug())
	if !strings.HasPrefix(p, episodesRoot+"/") {
		http.Error(w, "path must be under "+episodesRoot+"/", http.StatusBadRequest)
		return
	}
	if !strings.HasSuffix(p, episodeFileSuffix) {
		http.Error(w, "path must end in "+episodeFileSuffix, http.StatusBadRequest)
		return
	}

	body, err := store.Read(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "could not read episode: "+err.Error(), http.StatusInternalServerError)
		return
	}

	rel := strings.TrimPrefix(p, episodesRoot+"/")
	agent, name, _ := strings.Cut(rel, "/")
	if agent == "" || name == "" {
		http.Error(w, "path must be under "+episodesRoot+"/<agent>/", http.StatusBadRequest)
		return
	}

	content := string(body)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	retrieval := RetrievalScore{}
	if scorer := s.retrievalScorer(); scorer != nil {
		if scores, serr := scorer.ScoreEpisodes(r.Context(), s.activeProjectSlug(), agent, query, []string{p}); serr == nil {
			retrieval = scores[p]
		}
	}
	data := memoryEpisodeView{
		basePage: s.newBasePage("memory"),
		Agent:    agent,
		Name:     name,
		Path:     p,
		Content:  content,
		Tokens:   prompt.EstimateTokens(content),
		Query:    query,
		Indexed:  retrieval.Indexed,
		Score:    retrieval.Score,
		HasScore: retrieval.HasScore,
	}
	s.renderMemoryEpisodeView(w, data)
}

// validAgentName rejects empty names and any name that contains a path
// separator or the parent-dir token. The handler does not verify the
// agent exists in the registry - the goal here is to refuse traversal
// before the value reaches the memory reader.
func validAgentName(name string) bool {
	if name == "" {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return true
}

// listAgentEpisodes walks projects/global/episodes/<agent>/ and returns
// one row per .md file, sorted newest-first by filename. ISO 8601
// timestamps sort lexicographically, so a reverse string sort matches
// chronological order without parsing every filename. A missing agent
// directory yields an empty slice and no error so the page can render
// an empty-state hint rather than a 404.
func listAgentEpisodes(ctx context.Context, store MemoryStore, slug, agent, query string, scorer RetrievalScorer) ([]episodeRow, error) {
	dir := episodesRootForSlug(slug) + "/" + agent
	entries, err := store.Walk(dir)
	if err != nil {
		return nil, err
	}
	prefix := dir + "/"
	var rows []episodeRow
	var paths []string
	for _, e := range entries {
		if e.Dir {
			continue
		}
		if !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Path, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		if !strings.HasSuffix(e.Path, episodeFileSuffix) {
			continue
		}
		body, rerr := store.Read(e.Path)
		tokens := 0
		if rerr == nil {
			tokens = prompt.EstimateTokens(string(body))
		}
		rows = append(rows, episodeRow{
			Name:   path.Base(e.Path),
			Path:   e.Path,
			Tokens: tokens,
		})
		paths = append(paths, e.Path)
	}
	if scorer != nil && len(paths) > 0 {
		if scores, serr := scorer.ScoreEpisodes(ctx, slug, agent, query, paths); serr == nil {
			for i := range rows {
				if score, ok := scores[rows[i].Path]; ok {
					rows[i].Indexed = score.Indexed
					rows[i].Score = score.Score
					rows[i].HasScore = score.HasScore
				}
			}
		}
	}
	// Newest-first: ISO timestamps sort lexicographically, so a
	// descending sort by Name is the cheapest "newest first" we can
	// give the user without parsing each filename.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name > rows[j].Name })
	return rows, nil
}

// countEpisodesByAgent walks projects/global/episodes/ and returns one
// row per agent directory containing at least one .md file. The
// resulting slice is sorted by agent name so the /memory page renders
// stably across reloads.
func countEpisodesByAgent(store MemoryStore, slug string) ([]agentEpisodeCount, error) {
	episodesRoot := episodesRootForSlug(slug)
	entries, err := store.Walk(episodesRoot)
	if err != nil {
		return nil, err
	}
	prefix := episodesRoot + "/"
	counts := map[string]int{}
	for _, e := range entries {
		if e.Dir {
			continue
		}
		if !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		if !strings.HasSuffix(e.Path, episodeFileSuffix) {
			continue
		}
		rest := strings.TrimPrefix(e.Path, prefix)
		agent, name, ok := strings.Cut(rest, "/")
		if !ok || agent == "" || name == "" {
			continue
		}
		// Only count files directly under <agent>/, not nested
		// subdirectories the session writer doesn't create today.
		if strings.Contains(name, "/") {
			continue
		}
		counts[agent]++
	}
	if len(counts) == 0 {
		return nil, nil
	}
	out := make([]agentEpisodeCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, agentEpisodeCount{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func episodesRootForSlug(slug string) string {
	if slug == "" {
		slug = "global"
	}
	return "projects/" + slug + "/episodes"
}

// handleMemoryEdit renders the textarea form for one editable file.
// The set of editable paths is closed (see editableGlobalFiles), so any
// other path is rejected outright.
func (s *Server) handleMemoryEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	desc, ok := editableDesc(p)
	if !ok {
		http.Error(w, "not editable: "+p, http.StatusBadRequest)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not configured", http.StatusServiceUnavailable)
		return
	}

	data := memoryEditView{
		basePage: s.newBasePage("memory"),
		Path:     p,
		Desc:     desc,
	}
	b, err := store.Read(p)
	switch {
	case err == nil:
		data.Content = string(b)
		data.Tokens = prompt.EstimateTokens(data.Content)
	case errors.Is(err, fs.ErrNotExist):
		data.IsNew = true
	default:
		data.SaveErr = err.Error()
	}
	s.renderMemoryEdit(w, data)
}

// handleMemoryRebuildIndex triggers an idempotent index rebuild for
// episodes under the active project. The rebuild walks all .md episode
// files, re-embeds any SHA missing from the index, and updates the
// manifest. Completed episodes become findable through semantic search.
func (s *Server) handleMemoryRebuildIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rb := s.indexRebuilder()
	if rb == nil {
		http.Error(w, "index rebuild not available (embedder or index is not ready)", http.StatusServiceUnavailable)
		return
	}
	if err := rb.Rebuild(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("index rebuild failed: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/memory?rebuilt=1", http.StatusSeeOther)
}

// handleMemorySave persists the textarea content for one editable
// file. CRLF newlines are normalised to LF so the prompt assembler and
// git both see canonical line endings regardless of the user's OS.
func (s *Server) handleMemorySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	p := strings.TrimSpace(r.FormValue("path"))
	desc, ok := editableDesc(p)
	if !ok {
		http.Error(w, "not editable: "+p, http.StatusBadRequest)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not configured", http.StatusServiceUnavailable)
		return
	}

	content := strings.ReplaceAll(r.FormValue("content"), "\r\n", "\n")
	if len(content) > maxMemoryFileBytes {
		data := memoryEditView{
			basePage: s.newBasePage("memory"),
			Path:     p,
			Desc:     desc,
			Content:  content[:maxMemoryFileBytes],
			Tokens:   prompt.EstimateTokens(content),
			SaveErr:  "file too large to save (limit 1 MiB)",
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		s.renderMemoryEdit(w, data)
		return
	}

	if err := store.WriteFile(p, []byte(content)); err != nil {
		data := memoryEditView{
			basePage: s.newBasePage("memory"),
			Path:     p,
			Desc:     desc,
			Content:  content,
			Tokens:   prompt.EstimateTokens(content),
			SaveErr:  err.Error(),
		}
		w.WriteHeader(http.StatusInternalServerError)
		s.renderMemoryEdit(w, data)
		return
	}

	http.Redirect(w, r, "/memory?saved="+url.QueryEscape(p), http.StatusSeeOther)
}

func (s *Server) renderMemory(w http.ResponseWriter, data memoryView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.memoryTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) renderMemoryEdit(w http.ResponseWriter, data memoryEditView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.memoryEditTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) renderMemoryEpisodes(w http.ResponseWriter, data memoryEpisodesView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.memoryEpisodesTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) renderMemoryEpisodeView(w http.ResponseWriter, data memoryEpisodeView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.memoryEpisodeViewTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// buildMemoryTree walks the store, computes token estimates, and links
// child nodes onto their parents. Editable global/* files that are
// missing from disk are still injected as virtual nodes so the user
// can create them via the edit page without scaffolding first.
func buildMemoryTree(store MemoryStore) ([]*memoryTreeNode, int, error) {
	entries, err := store.Walk("")
	if err != nil {
		return nil, 0, err
	}
	// Sort by path so a parent dir is always processed before any of
	// its children. attachToParent looks the parent up in `nodes`, and
	// a missing parent forces the child into the root list - which
	// silently breaks directory token sums. DirReader.Walk happens to
	// sort already, but the interface doesn't promise it, so guard
	// here rather than rely on every implementation getting it right.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	nodes := make(map[string]*memoryTreeNode, len(entries))
	var roots []*memoryTreeNode

	for _, e := range entries {
		node := &memoryTreeNode{
			Name: path.Base(e.Path),
			Path: e.Path,
			Dir:  e.Dir,
		}
		if !e.Dir {
			if _, ok := editableDesc(e.Path); ok {
				node.Editable = true
			}
			b, rerr := store.Read(e.Path)
			if rerr == nil {
				node.Content = string(b)
				node.Tokens = prompt.EstimateTokens(node.Content)
			}
		}
		nodes[e.Path] = node
		attachToParent(node, nodes, &roots)
	}

	// Inject virtual placeholders for editable global files that don't
	// exist on disk yet, so the tree always offers an edit link for
	// them. The parent global/ dir is materialised on-the-fly when
	// missing too - otherwise a fresh repo would only show "agents".
	for _, f := range editableGlobalFiles {
		if _, exists := nodes[f.Path]; exists {
			continue
		}
		ensureParentDir(path.Dir(f.Path), nodes, &roots)
		node := &memoryTreeNode{
			Name:     path.Base(f.Path),
			Path:     f.Path,
			Editable: true,
			Missing:  true,
		}
		nodes[f.Path] = node
		attachToParent(node, nodes, &roots)
	}

	sortChildren(roots)

	total := 0
	for _, root := range roots {
		total += promptTokens(root)
	}
	return roots, total, nil
}

// attachToParent links node into its parent's Children slice, falling
// back to the roots list when the parent isn't in the tree (top-level
// entry, or a sibling whose parent the walker didn't return).
func attachToParent(node *memoryTreeNode, nodes map[string]*memoryTreeNode, roots *[]*memoryTreeNode) {
	parent := path.Dir(node.Path)
	if parent == "." || parent == "" {
		*roots = append(*roots, node)
		return
	}
	if p, ok := nodes[parent]; ok {
		p.Children = append(p.Children, node)
		return
	}
	*roots = append(*roots, node)
}

// ensureParentDir materialises a virtual directory node for relPath if
// none exists yet. Used so a missing global/ shows up as a parent for
// the virtual rules.md/user.md/facts.md placeholders.
func ensureParentDir(relPath string, nodes map[string]*memoryTreeNode, roots *[]*memoryTreeNode) {
	if relPath == "." || relPath == "" {
		return
	}
	if _, ok := nodes[relPath]; ok {
		return
	}
	ensureParentDir(path.Dir(relPath), nodes, roots)
	dir := &memoryTreeNode{
		Name:    path.Base(relPath),
		Path:    relPath,
		Dir:     true,
		Missing: true,
	}
	nodes[relPath] = dir
	attachToParent(dir, nodes, roots)
}

// sortChildren sorts each directory's children: dirs first, then files,
// each group lexicographic by name. Walk already returns sorted paths
// but injected virtual nodes can land at the end, so we re-sort.
func sortChildren(nodes []*memoryTreeNode) {
	for _, n := range nodes {
		if len(n.Children) == 0 {
			continue
		}
		// Bubble sort is fine here: a memory tree has tens of nodes,
		// and pulling in sort.Slice for each level just to compare
		// two booleans plus a string is overkill.
		for i := 1; i < len(n.Children); i++ {
			for j := i; j > 0 && treeLess(n.Children[j], n.Children[j-1]); j-- {
				n.Children[j], n.Children[j-1] = n.Children[j-1], n.Children[j]
			}
		}
		sortChildren(n.Children)
	}
}

func treeLess(a, b *memoryTreeNode) bool {
	if a.Dir != b.Dir {
		return a.Dir
	}
	return a.Name < b.Name
}

// promptTokens fills in Tokens for directories and returns the value
// for n. Most directories sum their file descendants. The top-level
// agents/ dir is the exception: only one agent's content is loaded
// into any single prompt, so its token total is the largest single
// agent rather than the sum across all agents. Subdirectories under
// agents/<name>/ still sum normally - the special case applies only at
// the agents/ root.
func promptTokens(n *memoryTreeNode) int {
	if !n.Dir {
		return n.Tokens
	}
	if n.Path == agentsDirName {
		biggest := 0
		for _, c := range n.Children {
			if t := promptTokens(c); t > biggest {
				biggest = t
			}
		}
		n.Tokens = biggest
		return biggest
	}
	total := 0
	for _, c := range n.Children {
		total += promptTokens(c)
	}
	n.Tokens = total
	return total
}
