package ui

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/VrncQuentin/harness/internal/memory"
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

type stubRetrievalScorer struct {
	query  string
	paths  []string
	scores map[string]RetrievalScore
}

type stubDedupChecker struct {
	blocked      bool
	similar      string
	score        float64
	err          error
	gotText      string
	gotThreshold float64
}

func (s *stubDedupChecker) CheckSimilar(_ context.Context, text string, threshold float64) (bool, string, float64, error) {
	s.gotText = text
	s.gotThreshold = threshold
	return s.blocked, s.similar, s.score, s.err
}

type stubCommitter struct {
	err      error
	messages []string
	files    [][]string
}

func (s *stubCommitter) Commit(msg string, files []string) (string, error) {
	s.messages = append(s.messages, msg)
	s.files = append(s.files, append([]string(nil), files...))
	if s.err != nil {
		return "", s.err
	}
	return "abc123", nil
}

func (s *stubRetrievalScorer) ScoreEpisodes(_ context.Context, query string, paths []string) (map[string]RetrievalScore, error) {
	s.query = query
	s.paths = append([]string(nil), paths...)
	return s.scores, nil
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
func (s *stubMemoryStore) RemoveAll(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, p)
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
	if !strings.Contains(rec.Body.String(), "Memory repo not ready") {
		t.Errorf("expected setup CTA, got:\n%s", rec.Body.String())
	}
}

func TestHandleMemory_RendersTreeAndTokens(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"rules.md":                "be helpful",
		"user.md":                 "the user is named alice",
		"agents/coder/persona.md": "you are a coder",
	})
	setMemoryStoreForTest(s, store)
	setMemoryRepoPathForTest(s, "C:\\repo")

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
		`href="/memory/edit?path=rules.md"`,
		`href="/memory/edit?path=facts.md"`,
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

func TestHandleMemory_InlinesFileContent(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"rules.md":                "be helpful and terse",
		"agents/coder/persona.md": "you are a coder",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		// File bodies are inlined inside the expandable <details> block
		// so the user can read them without leaving /memory.
		"be helpful and terse",
		"you are a coder",
		`<details class="tree-file">`,
		`<pre class="tree-content">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("memory body missing %q", want)
		}
	}
}

func TestHandleMemory_EmptyFileShowsHint(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"rules.md": "",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "(empty file)") {
		t.Errorf("expected empty-file hint, got:\n%s", body)
	}
}

func TestHandleMemory_ShowsSavedFlash(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory?saved=rules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Saved") || !strings.Contains(body, "rules.md") {
		t.Errorf("expected saved flash, got:\n%s", body)
	}
}

func TestHandleMemory_SurfacesWalkError(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	store.walkErr = errors.New("disk on fire")
	setMemoryStoreForTest(s, store)

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

func TestHandlePromoteFact_DedupBlockedSkipsWriteAndCommit(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"facts.md": "existing fact\n",
	})
	committer := &stubCommitter{}
	checker := &stubDedupChecker{blocked: true, similar: "existing fact", score: 0.972}
	setMemoryStoreForTest(s, store)
	setCommitterForTest(s, committer)
	setDedupCheckerForTest(s, checker)
	setPromotionDedupThresholdForTest(s, 0.95)

	form := url.Values{}
	form.Set("text", "existing fact with punctuation")
	req := httptest.NewRequest(http.MethodPost, "/memory/promote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handlePromoteFact(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "dedup=1") || !strings.Contains(loc, "similar=existing+fact") || !strings.Contains(loc, "score=0.97") {
		t.Fatalf("unexpected redirect location %q", loc)
	}
	if checker.gotText != "existing fact with punctuation" || checker.gotThreshold != 0.95 {
		t.Fatalf("dedup checker got text=%q threshold=%v", checker.gotText, checker.gotThreshold)
	}
	if store.lastWritePath != "" {
		t.Fatalf("dedup-blocked promotion wrote %s", store.lastWritePath)
	}
	if len(committer.messages) != 0 {
		t.Fatalf("dedup-blocked promotion committed: %#v", committer.messages)
	}
}

func TestHandlePromoteFact_UsesActiveProjectStore(t *testing.T) {
	s := NewServer(3000)
	activeStore := newStubMemoryStore(map[string]string{
		"facts.md": "active fact\n",
	})
	globalStore := newStubMemoryStore(map[string]string{
		"facts.md": "global fact\n",
	})
	committer := &stubCommitter{}
	s.SetServiceDeps(ServiceDeps{MemoryStore: activeStore, Committer: committer})

	form := url.Values{}
	form.Set("text", "new project fact")
	req := httptest.NewRequest(http.MethodPost, "/memory/promote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handlePromoteFact(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if activeStore.lastWritePath != "facts.md" || !strings.Contains(string(activeStore.lastWriteData), "new project fact") {
		t.Fatalf("active store was not updated: path=%q data=%q", activeStore.lastWritePath, string(activeStore.lastWriteData))
	}
	if globalStore.lastWritePath != "" {
		t.Fatalf("global store was updated: path=%q", globalStore.lastWritePath)
	}
	if len(committer.files) != 1 || len(committer.files[0]) != 1 || committer.files[0][0] != "facts.md" {
		t.Fatalf("commit files = %#v, want facts.md", committer.files)
	}
}
func TestHandlePromoteFact_CommitErrorReturns500(t *testing.T) {
	s := NewServer(3000)
	setMemoryStoreForTest(s, newStubMemoryStore(map[string]string{
		"facts.md": "existing fact\n",
	}))
	setCommitterForTest(s, &stubCommitter{err: errors.New("git offline")})

	form := url.Values{}
	form.Set("text", "new fact")
	req := httptest.NewRequest(http.MethodPost, "/memory/promote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handlePromoteFact(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "commit fact") {
		t.Errorf("expected commit error in body, got %q", rec.Body.String())
	}
	store := s.memoryStore().(*stubMemoryStore)
	if got := store.files["facts.md"]; got != "existing fact\n" {
		t.Fatalf("facts.md after commit failure = %q", got)
	}
}

func TestHandleAppendNote_CommitErrorReturns500(t *testing.T) {
	s := NewServer(3000)
	setMemoryStoreForTest(s, newStubMemoryStore(map[string]string{
		"agents/coder/notes.md": "existing note\n",
	}))
	setCommitterForTest(s, &stubCommitter{err: errors.New("git offline")})

	form := url.Values{}
	form.Set("agent", "coder")
	form.Set("text", "new note")
	req := httptest.NewRequest(http.MethodPost, "/memory/note", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAppendNote(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "commit note") {
		t.Errorf("expected commit error in body, got %q", rec.Body.String())
	}
	store := s.memoryStore().(*stubMemoryStore)
	if got := store.files["agents/coder/notes.md"]; got != "existing note\n" {
		t.Fatalf("notes.md after commit failure = %q", got)
	}
}

func TestHandleMemoryEdit_RendersExistingContent(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"rules.md": "be terse",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=rules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"be terse", `name="path" value="rules.md"`, `action="/memory/save"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit body missing %q", want)
		}
	}
}

func TestHandleMemoryEdit_NewFileShowsBlankForm(t *testing.T) {
	s := NewServer(3000)
	setMemoryStoreForTest(s, newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=facts.md", nil)
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
	setMemoryStoreForTest(s, newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=agents/coder/persona.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-editable path, got %d", rec.Code)
	}
}

func TestHandleMemoryEdit_NoStoreReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/memory/edit?path=rules.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEdit(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no store, got %d", rec.Code)
	}
}

func TestHandleMemorySave_PersistsAndRedirects(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	setMemoryStoreForTest(s, store)

	form := url.Values{}
	form.Set("path", "rules.md")
	form.Set("content", "line one\r\nline two\r\n")

	req := httptest.NewRequest(http.MethodPost, "/memory/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMemorySave(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/memory?saved=rules.md"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := store.lastWritePath; got != "rules.md" {
		t.Errorf("Write path = %q, want rules.md", got)
	}
	// CRLF must be normalised before persisting.
	if got := string(store.lastWriteData); got != "line one\nline two\n" {
		t.Errorf("Write data = %q, want LF-normalised", got)
	}
}

func TestHandleMemorySave_RejectsNonEditable(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	setMemoryStoreForTest(s, store)

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
	setMemoryStoreForTest(s, store)

	form := url.Values{}
	form.Set("path", "rules.md")
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
	setMemoryStoreForTest(s, store)

	form := url.Values{}
	form.Set("path", "rules.md")
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
		"rules.md": "x",
	})
	tree, _, err := buildMemoryTree(store)
	if err != nil {
		t.Fatalf("buildMemoryTree: %v", err)
	}
	have := map[string]bool{}
	for _, n := range tree {
		have[n.Path] = n.Missing
	}
	if missing, ok := have["user.md"]; !ok || !missing {
		t.Errorf("expected user.md as a missing virtual node; map=%v", have)
	}
	if missing, ok := have["facts.md"]; !ok || !missing {
		t.Errorf("expected facts.md as a missing virtual node; map=%v", have)
	}
	if missing, ok := have["rules.md"]; !ok || missing {
		t.Errorf("expected rules.md to be present and not missing; map=%v", have)
	}
}
func TestBuildMemoryTree_DirTokensAreSumOfChildren(t *testing.T) {
	store := newStubMemoryStore(map[string]string{
		// rune count 8 → 2 tokens; rune count 16 → 4 tokens
		"rules.md": "abcdefgh",
		"user.md":  "abcdefghijklmnop",
	})
	_, total, err := buildMemoryTree(store)
	if err != nil {
		t.Fatalf("buildMemoryTree: %v", err)
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
		"rules.md":                   "abcdefgh",              // 2
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

func TestHandleMemoryEpisodes_ListsNewestFirst(t *testing.T) {
	s := NewServer(3000)
	// Three episode files for the coder agent at different timestamps.
	// ISO 8601 timestamps sort lexicographically, so a reverse sort
	// yields chronological newest-first.
	store := newStubMemoryStore(map[string]string{
		"episodes/coder/2026-04-20T10:00:00Z.md": "first",
		"episodes/coder/2026-04-22T11:30:00Z.md": "middle",
		"episodes/coder/2026-04-25T09:15:00Z.md": "newest",
		// Files for another agent must not leak into the coder list.
		"episodes/reviewer/2026-04-19T08:00:00Z.md": "other",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent=coder", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"2026-04-20T10:00:00Z.md",
		"2026-04-22T11:30:00Z.md",
		"2026-04-25T09:15:00Z.md",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("episodes body missing %q", want)
		}
	}
	// Reviewer's episode must not appear in the coder list.
	if strings.Contains(body, "2026-04-19T08:00:00Z.md") {
		t.Error("reviewer episode leaked into coder listing")
	}
	// Newest must appear before middle, which must appear before oldest.
	idxNewest := strings.Index(body, "2026-04-25T09:15:00Z.md")
	idxMiddle := strings.Index(body, "2026-04-22T11:30:00Z.md")
	idxOldest := strings.Index(body, "2026-04-20T10:00:00Z.md")
	if idxNewest >= idxMiddle || idxMiddle >= idxOldest {
		t.Errorf("expected newest-first ordering; got positions newest=%d middle=%d oldest=%d", idxNewest, idxMiddle, idxOldest)
	}
}

func TestHandleMemoryEpisodes_UsesActiveProject(t *testing.T) {
	s := NewServer(3000)
	s.SetProjectDirectoryWarnings("dt", nil)
	activeStore := newStubMemoryStore(map[string]string{
		"episodes/coder/dt.md": "project",
	})
	s.SetServiceDeps(ServiceDeps{MemoryStore: activeStore})

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent=coder", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "dt.md") {
		t.Errorf("active project episode missing from body:\n%s", body)
	}
	if strings.Contains(body, "global.md") {
		t.Errorf("global episode leaked into active project listing:\n%s", body)
	}
}
func TestHandleMemoryEpisodes_RendersRetrievalScores(t *testing.T) {
	s := NewServer(3000)
	path := "episodes/coder/2026-04-25T09:15:00Z.md"
	store := newStubMemoryStore(map[string]string{path: "episode"})
	setMemoryStoreForTest(s, store)
	scorer := &stubRetrievalScorer{scores: map[string]RetrievalScore{
		path: {Indexed: true, Score: 0.75, HasScore: true},
	}}
	setRetrievalScorerForTest(s, scorer)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent=coder&q=needle", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"score 0.750", "indexed", `name="q" value="needle"`} {
		if !strings.Contains(body, want) {
			t.Errorf("episodes body missing %q", want)
		}
	}
	if scorer.query != "needle" {
		t.Errorf("scorer query = %q", scorer.query)
	}
	if len(scorer.paths) != 1 || scorer.paths[0] != path {
		t.Errorf("scorer paths = %v, want [%s]", scorer.paths, path)
	}
}

func TestHandleMemoryEpisodes_EmptyDirShowsHint(t *testing.T) {
	s := NewServer(3000)
	// No episodes at all: the agent dir does not exist on disk.
	store := newStubMemoryStore(nil)
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent=coder", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No episodes yet for") {
		t.Errorf("expected empty-state hint, got:\n%s", body)
	}
}

func TestHandleMemoryEpisodes_RejectsTraversalInAgent(t *testing.T) {
	tests := []struct {
		name  string
		agent string
	}{
		{"empty", ""},
		{"dotdot", ".."},
		{"single dot", "."},
		{"forward slash", "a/b"},
		{"backslash", `a\b`},
		{"traversal slash", "../etc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(3000)
			setMemoryStoreForTest(s, newStubMemoryStore(nil))

			req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent="+url.QueryEscape(tc.agent), nil)
			rec := httptest.NewRecorder()
			s.handleMemoryEpisodes(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("agent=%q: expected 400, got %d (body: %s)", tc.agent, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleMemoryEpisodes_NoStoreReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes?agent=coder", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no store, got %d", rec.Code)
	}
}

func TestHandleMemoryEpisodes_RejectsPOST(t *testing.T) {
	s := NewServer(3000)
	setMemoryStoreForTest(s, newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodPost, "/memory/episodes?agent=coder", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodes(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestHandleMemoryEpisodeView_RendersContent(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"episodes/coder/2026-04-25T09:15:00Z.md": "## Episode body\nSome notes.",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes/view?path=episodes/coder/2026-04-25T09:15:00Z.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodeView(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"## Episode body",
		"Some notes.",
		"2026-04-25T09:15:00Z.md",
		`href="/memory/episodes?agent=coder"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("episode view missing %q", want)
		}
	}
}

func TestHandleMemoryEpisodeView_RejectsNonEpisodePath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"outside episodes root", "rules.md"},
		{"agents tree", "agents/coder/persona.md"},
		{"different project root", "projects/other/episodes/coder/x.md"},
		{"traversal", "episodes/coder/../../etc/passwd.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(3000)
			setMemoryStoreForTest(s, newStubMemoryStore(nil))

			req := httptest.NewRequest(http.MethodGet, "/memory/episodes/view?path="+url.QueryEscape(tc.path), nil)
			rec := httptest.NewRecorder()
			s.handleMemoryEpisodeView(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("path=%q: expected 400, got %d (body: %s)", tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleMemoryEpisodeView_RejectsNonMarkdownSuffix(t *testing.T) {
	s := NewServer(3000)
	setMemoryStoreForTest(s, newStubMemoryStore(nil))

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes/view?path=episodes/coder/notes.txt", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodeView(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-.md path, got %d", rec.Code)
	}
}

func TestHandleMemoryEpisodeView_MissingFileReturns404(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(nil)
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes/view?path=episodes/coder/2026-04-25T09:15:00Z.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodeView(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing file, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestHandleMemoryEpisodeView_NoStoreReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/memory/episodes/view?path=episodes/coder/x.md", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryEpisodeView(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no store, got %d", rec.Code)
	}
}

func TestHandleMemory_RendersEpisodesByAgent(t *testing.T) {
	s := NewServer(3000)
	store := newStubMemoryStore(map[string]string{
		"episodes/coder/2026-04-20T10:00:00Z.md":    "a",
		"episodes/coder/2026-04-22T11:30:00Z.md":    "b",
		"episodes/reviewer/2026-04-19T08:00:00Z.md": "c",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Episodes by agent",
		"coder",
		"reviewer",
		"2 episodes",
		"1 episode",
		// html/template percent-encodes "/" in href attributes; the
		// browser decodes it back, so the route still matches.
		`href="/memory/episodes?agent=coder"`,
		`href="/memory/episodes?agent=reviewer"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("memory body missing %q", want)
		}
	}
}

func TestHandleMemory_EmptyEpisodesShowsHint(t *testing.T) {
	s := NewServer(3000)
	// Repo has global content but no episodes anywhere.
	store := newStubMemoryStore(map[string]string{
		"rules.md": "x",
	})
	setMemoryStoreForTest(s, store)

	req := httptest.NewRequest(http.MethodGet, "/memory", nil)
	rec := httptest.NewRecorder()
	s.handleMemory(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "No sessions saved yet") {
		t.Errorf("expected empty-episodes hint, got:\n%s", body)
	}
}
