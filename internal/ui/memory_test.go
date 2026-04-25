package ui

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vrnc/harness/internal/memory"
)

// stubMemoryStore is a fake MemoryStore backed by a map for the
// /memory page tests. It records the last Write so assertions can
// verify the handler persisted the right bytes.
type stubMemoryStore struct {
	mu    sync.Mutex
	files map[string]string

	walkErr  error
	readErr  error
	writeErr error

	lastWritePath string
	lastWriteData []byte
}

func newStubMemoryStore(files map[string]string) *stubMemoryStore {
	cp := make(map[string]string, len(files))
	for k, v := range files {
		cp[k] = v
	}
	return &stubMemoryStore{files: cp}
}

func (s *stubMemoryStore) Walk(_ string) ([]memory.Entry, error) {
	if s.walkErr != nil {
		return nil, s.walkErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := map[string]struct{}{}
	for p := range s.files {
		segs := strings.Split(p, "/")
		for i := 1; i < len(segs); i++ {
			dirs[strings.Join(segs[:i], "/")] = struct{}{}
		}
	}
	out := make([]memory.Entry, 0, len(s.files)+len(dirs))
	for d := range dirs {
		out = append(out, memory.Entry{Path: d, Dir: true})
	}
	for p, body := range s.files {
		out = append(out, memory.Entry{Path: p, Size: int64(len(body))})
	}
	return out, nil
}

func (s *stubMemoryStore) Read(p string) ([]byte, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(body), nil
}

func (s *stubMemoryStore) WriteFile(p string, data []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string]string{}
	}
	s.files[p] = string(data)
	s.lastWritePath = p
	s.lastWriteData = append([]byte(nil), data...)
	return nil
}

func TestHandleMemory_NoStoreShowsCTA(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Memory repo not configured") {
		t.Errorf("expected setup CTA, got:\n%s", rec.Body.String())
	}
}

func TestHandleMemory_RendersTreeAndTokens(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"global/rules.md":         "be helpful",
		"global/user.md":          "the user is named alice",
		"agents/coder/persona.md": "you are a coder",
	})
	s.SetMemoryStore(store)
	s.SetMemoryRepoPath("C:\\repo")

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"rules.md",
		"user.md",
		"facts.md", // virtual: missing but injected for the editor
		"persona.md",
		// html/template percent-encodes "/" in href attributes; the
		// browser decodes it back, so the route still matches.
		`href="/memory/edit?path=global%2frules.md"`,
		`href="/memory/edit?path=global%2ffacts.md"`,
		"C:\\repo",
		filepath.Join("C:\\repo", "agents"),
		"biggest agent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("memory body missing %q", want)
		}
	}
	// Persona files are not editable from the UI, so no edit link there.
	if strings.Contains(body, "/memory/edit?path=agents") {
		t.Error("persona.md should not be editable from /memory")
	}
}

func TestHandleMemory_ShowsSavedFlash(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	s.SetMemoryStore(store)

	req := httptest.NewRequest(http.MethodGet, "/memory?saved=global%2Frules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Saved") || !strings.Contains(body, "global/rules.md") {
		t.Errorf("expected saved flash, got:\n%s", body)
	}
}

func TestHandleMemory_SurfacesWalkError(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	store.walkErr = errors.New("disk on fire")
	s.SetMemoryStore(store)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "disk on fire") {
		t.Errorf("expected walk error in body, got:\n%s", body)
	}
}

func TestHandleMemory_RejectsPOST(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodPost, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestHandleMemoryEdit_RendersExistingContent(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"global/rules.md": "be terse",
	})
	s.SetMemoryStore(store)

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=global/rules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"be terse", `name="path" value="global/rules.md"`, `action="/memory/save"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit body missing %q", want)
		}
	}
}

func TestHandleMemoryEdit_NewFileShowsBlankForm(t *testing.T) {
	s := NewServer(3000)
	s.SetMemoryStore(newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=global/facts.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "new file") {
		t.Errorf("expected 'new file' marker for missing global file, got:\n%s", body)
	}
}

func TestHandleMemoryEdit_RejectsNonEditablePath(t *testing.T) {
	s := NewServer(3000)
	s.SetMemoryStore(newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=agents/coder/persona.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-editable path, got %d", rec.Code)
	}
}

func TestHandleMemoryEdit_NoStoreReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=global/rules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no store, got %d", rec.Code)
	}
}

func TestHandleMemorySave_PersistsAndRedirects(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	s.SetMemoryStore(store)

	form := url.Values{}
	form.Set("path", "global/rules.md")
	form.Set("content", "line one\r\nline two\r\n")

	req := httptest.NewRequest(http.MethodPost, "/memory/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/memory?saved=global%2Frules.md"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := store.lastWritePath; got != "global/rules.md" {
		t.Errorf("Write path = %q, want global/rules.md", got)
	}
	// CRLF must be normalised before persisting.
	if got := string(store.lastWriteData); got != "line one\nline two\n" {
		t.Errorf("Write data = %q, want LF-normalised", got)
	}
}

func TestHandleMemorySave_RejectsNonEditable(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	s.SetMemoryStore(store)

	form := url.Values{}
	form.Set("path", "agents/coder/persona.md")
	form.Set("content", "x")

	req := httptest.NewRequest(http.MethodPost, "/memory/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-editable path, got %d", rec.Code)
	}
	if store.lastWritePath != "" {
		t.Errorf("Write should not have been called, got path=%q", store.lastWritePath)
	}
}

func TestHandleMemorySave_TooLargeRejected(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	s.SetMemoryStore(store)

	form := url.Values{}
	form.Set("path", "global/rules.md")
	form.Set("content", strings.Repeat("a", maxMemoryFileBytes+1))

	req := httptest.NewRequest(http.MethodPost, "/memory/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversize, got %d", rec.Code)
	}
	if store.lastWritePath != "" {
		t.Error("oversize submit should not have called Write")
	}
}

func TestHandleMemorySave_StoreErrorRendersForm(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	store.writeErr = errors.New("disk full")
	s.SetMemoryStore(store)

	form := url.Values{}
	form.Set("path", "global/rules.md")
	form.Set("content", "x")

	req := httptest.NewRequest(http.MethodPost, "/memory/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Error("expected store error in re-rendered form")
	}
}

func TestHandleMemorySave_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/memory/save", nil)
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestBuildMemoryTree_InjectsMissingGlobals(t *testing.T) {
	store := newStubMemoryStore(map[string]string{
		"global/rules.md": "x",
	})
	tree, _, err := buildMemoryTree(store)
	if err != nil {
		t.Fatalf("buildMemoryTree: %v", err)
	}
	// Find the global dir node.
	var global *memoryTreeNode
	for _, n := range tree {
		if n.Path == "global" {
			global = n
			break
		}
	}
	if global == nil {
		t.Fatal("expected global/ in the rendered tree")
	}
	have := map[string]bool{}
	for _, c := range global.Children {
		have[c.Path] = c.Missing
	}
	if missing, ok := have["global/user.md"]; !ok || !missing {
		t.Errorf("expected global/user.md as a missing virtual node; map=%v", have)
	}
	if missing, ok := have["global/facts.md"]; !ok || !missing {
		t.Errorf("expected global/facts.md as a missing virtual node; map=%v", have)
	}
	if missing, ok := have["global/rules.md"]; !ok || missing {
		t.Errorf("expected global/rules.md to be present and not missing; map=%v", have)
	}
}

func TestBuildMemoryTree_DirTokensAreSumOfChildren(t *testing.T) {
	store := newStubMemoryStore(map[string]string{
		// rune count 8 → 2 tokens; rune count 16 → 4 tokens
		"global/rules.md": "abcdefgh",
		"global/user.md":  "abcdefghijklmnop",
	})
	tree, total, err := buildMemoryTree(store)
	if err != nil {
		t.Fatalf("buildMemoryTree: %v", err)
	}
	var global *memoryTreeNode
	for _, n := range tree {
		if n.Path == "global" {
			global = n
			break
		}
	}
	if global == nil {
		t.Fatal("expected global/ in tree")
	}
	if global.Tokens != 6 {
		t.Errorf("global Tokens = %d, want 6 (2+4)", global.Tokens)
	}
	if total != 6 {
		t.Errorf("total Tokens = %d, want 6", total)
	}
}

func TestBuildMemoryTree_AgentsDirUsesBiggestAgent(t *testing.T) {
	// Two agents at very different sizes. The page total should
	// reflect global + the biggest single agent, not the sum across
	// agents, since only one agent runs in any given prompt.
	// rune-quarter tokens: 8→2, 16→4, 40→10.
	store := newStubMemoryStore(map[string]string{
		"global/rules.md":            "abcdefgh",              // 2
		"agents/small/persona.md":    "abcdefghijklmnop",      // 4
		"agents/big/persona.md":      strings.Repeat("a", 40), // 10
		"agents/big/episodes/one.md": strings.Repeat("b", 40), // 10
	})
	tree, total, err := buildMemoryTree(store)
	if err != nil {
		t.Fatalf("buildMemoryTree: %v", err)
	}

	var agents *memoryTreeNode
	for _, n := range tree {
		if n.Path == "agents" {
			agents = n
			break
		}
	}
	if agents == nil {
		t.Fatal("expected agents/ in tree")
	}
	// big = 10 + 10 = 20; small = 4. Max is 20.
	if agents.Tokens != 20 {
		t.Errorf("agents Tokens = %d, want 20 (max of 20, 4)", agents.Tokens)
	}
	// total = global (2) + biggest agent (20) = 22.
	if total != 22 {
		t.Errorf("total Tokens = %d, want 22 (global 2 + biggest agent 20)", total)
	}

	// Subdirectories under an individual agent still sum normally.
	var bigAgent *memoryTreeNode
	for _, c := range agents.Children {
		if c.Path == "agents/big" {
			bigAgent = c
			break
		}
	}
	if bigAgent == nil {
		t.Fatal("expected agents/big in tree")
	}
	if bigAgent.Tokens != 20 {
		t.Errorf("agents/big Tokens = %d, want 20 (10+10)", bigAgent.Tokens)
	}
}

func TestSetMemoryStore_Roundtrip(t *testing.T) {
	s := NewServer(3000)
	if got := s.memoryStore(); got != nil {
		t.Errorf("default memoryStore should be nil, got %v", got)
	}
	store := newStubMemoryStore(nil)
	s.SetMemoryStore(store)
	if got := s.memoryStore(); got != store {
		t.Errorf("memoryStore after set: got %v, want %v", got, store)
	}
	s.SetMemoryStore(nil)
	if got := s.memoryStore(); got != nil {
		t.Errorf("memoryStore after clear: got %v, want nil", got)
	}
}
