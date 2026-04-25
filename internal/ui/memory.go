package ui

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/prompt"
)

// maxMemoryFileBytes caps the size of a file the editor will accept on
// save. Memory files are hand-authored markdown - anything larger is
// almost certainly a paste accident or a wrong path. Reject early so
// the user gets a clear error instead of a silent truncation later.
const maxMemoryFileBytes = 1 << 20 // 1 MiB

// MemoryStore is the surface the /memory page needs from the memory
// repo. Implemented by *memory.DirReader; broken out so tests can stub
// the FS without spinning up a real DirReader.
type MemoryStore interface {
	Walk(relPath string) ([]memory.Entry, error)
	Read(relPath string) ([]byte, error)
	Write(relPath string, data []byte) error
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

// memoryView is the template context for /memory.
type memoryView struct {
	basePage
	Configured  bool
	RepoPath    string
	Tree        []*memoryTreeNode
	TotalTokens int
	LoadErr     string
	SavedPath   string // flash: file just saved
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
		tree, total, err := buildMemoryTree(store)
		if err != nil {
			data.LoadErr = err.Error()
		}
		data.Tree = tree
		data.TotalTokens = total
	}
	if saved := strings.TrimSpace(r.URL.Query().Get("saved")); saved != "" {
		data.SavedPath = saved
	}
	s.renderMemory(w, data)
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

	if err := store.Write(p, []byte(content)); err != nil {
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

// buildMemoryTree walks the store, computes token estimates, and links
// child nodes onto their parents. Editable global/* files that are
// missing from disk are still injected as virtual nodes so the user
// can create them via the edit page without scaffolding first.
func buildMemoryTree(store MemoryStore) ([]*memoryTreeNode, int, error) {
	entries, err := store.Walk("")
	if err != nil {
		return nil, 0, err
	}
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
				node.Tokens = prompt.EstimateTokens(string(b))
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
		total += sumTokens(root)
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

// sumTokens fills in Tokens for directories as the sum of their file
// descendants and returns the total for n. Missing virtual files
// contribute zero (they have no content yet).
func sumTokens(n *memoryTreeNode) int {
	if !n.Dir {
		return n.Tokens
	}
	total := 0
	for _, c := range n.Children {
		total += sumTokens(c)
	}
	n.Tokens = total
	return total
}
