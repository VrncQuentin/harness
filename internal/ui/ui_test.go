package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/db"
	"github.com/VrncQuentin/harness/internal/logbuf"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
)

func testDefaultMemoryRepoPath(root string) db.DefaultMemoryRepoPathFunc {
	return func(slug string) (string, error) {
		return filepath.Join(root, "projects", slug), nil
	}
}

// newServerWithStore returns a Server wired to a fresh temp SQLite config store.
// The store is also returned for assertions.
func newServerWithStore(t *testing.T) (*Server, config.Store) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "harness.db"), testDefaultMemoryRepoPath(dir))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	store := d.Config()
	s := NewServer(3000)
	s.SetConfigStore(store)
	return s, store
}

type countingProjectStore struct {
	projects    []project.Project
	listCalls   int32
	hiddenCalls int32
	hiddenSlug  string
	hiddenValue bool
}

func (s *countingProjectStore) List(bool) ([]project.Project, error) {
	atomic.AddInt32(&s.listCalls, 1)
	return append([]project.Project(nil), s.projects...), nil
}

func (s *countingProjectStore) Get(string) (project.Project, error) {
	return project.Project{}, errors.New("not implemented")
}

func (s *countingProjectStore) Create(project.CreateInput) (project.Project, error) {
	return project.Project{}, errors.New("not implemented")
}

func (s *countingProjectStore) Update(project.UpdateInput) (project.Project, error) {
	return project.Project{}, errors.New("not implemented")
}

func (s *countingProjectStore) SetHidden(slug string, hidden bool) error {
	atomic.AddInt32(&s.hiddenCalls, 1)
	s.hiddenSlug = slug
	s.hiddenValue = hidden
	return nil
}

func (s *countingProjectStore) ListDirectories(string) ([]project.Directory, error) {
	return nil, nil
}

type stubConfigStore struct {
	cfg config.Config
}

func (s *stubConfigStore) Load() (*config.Config, bool, error) {
	cfg := s.cfg
	return &cfg, true, nil
}

func (s *stubConfigStore) Save(cfg *config.Config) error {
	if cfg != nil {
		s.cfg = *cfg
	}
	return nil
}

func (s *stubConfigStore) SetActiveProjectSlug(slug string) error {
	s.cfg.Project.ActiveProjectSlug = slug
	return nil
}

type noopIndexRebuilder struct{}

func (noopIndexRebuilder) Rebuild(context.Context) error { return nil }

// staticSnapshotProvider serves one fixed snapshot with a no-op lease. It is
// the test stand-in for the runtime's generation-bound snapshot provider.
type staticSnapshotProvider struct {
	snap ServiceDeps
}

func (p *staticSnapshotProvider) AcquireUISnapshot() (ServiceDeps, func()) {
	return p.snap, func() {}
}

func currentSnapshotForTest(s *Server) ServiceDeps {
	snap, _ := s.acquireSnapshot()
	return snap
}

func setServiceDepsForTest(s *Server, mut func(*ServiceDeps)) {
	deps := currentSnapshotForTest(s)
	mut(&deps)
	s.SetSnapshotProvider(&staticSnapshotProvider{snap: deps})
}

func setSnapshotForTest(s *Server, deps ServiceDeps) {
	s.SetSnapshotProvider(&staticSnapshotProvider{snap: deps})
}

func setAgentRegistryForTest(s *Server, reg AgentRegistry) {
	setServiceDepsForTest(s, func(d *ServiceDeps) {
		d.AgentRegistry = reg
		// Mirror the production acquisition-scoped active agent so the
		// rendered chat/agents pages highlight the same selection the registry
		// was configured with.
		d.ActiveAgent = reg.Active()
	})
}

func setChatRunnerForTest(s *Server, runner ChatRunner) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.ChatRunner = runner })
}

func setMemoryStoreForTest(s *Server, store MemoryStore) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.MemoryStore = store })
}

func setRetrievalScorerForTest(s *Server, scorer RetrievalScorer) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.RetrievalScorer = scorer })
}

func setCommitterForTest(s *Server, committer Committer) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.Committer = committer })
}

func setDedupCheckerForTest(s *Server, checker DedupChecker) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.Dedup = checker })
}

func setPromotionDedupThresholdForTest(s *Server, threshold float64) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.PromotionDedupThreshold = threshold })
}

func setSessionStoreForTest(s *Server, store SessionStore) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.SessionStore = store })
}

func setTaskRunnerForTest(s *Server, runner TaskRunner) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.TaskRunner = runner })
}

func setMemoryRepoPathForTest(s *Server, path string) {
	setServiceDepsForTest(s, func(d *ServiceDeps) { d.MemoryRepoPath = path })
}
func TestSnapshotProviderPublishesAndClearsDeps(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("coder", AgentInfo{Name: "coder"})
	memStore := newStubMemoryStore(nil)
	sessionStore := &stubSessionStore{}
	committer := &stubCommitter{}
	dedup := &stubDedupChecker{}
	scorer := &stubRetrievalScorer{}
	rebuilder := noopIndexRebuilder{}
	chatRunner := &stubChatRunner{}
	taskRunner := &recordingTaskRunner{}

	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		MemoryRepoPath:          "C:\\repo",
		AgentRegistry:           reg,
		MemoryStore:             memStore,
		SessionStore:            sessionStore,
		Committer:               committer,
		Dedup:                   dedup,
		PromotionDedupThreshold: 0.95,
		RetrievalScorer:         scorer,
		IndexRebuilder:          rebuilder,
		ChatRunner:              chatRunner,
		TaskRunner:              taskRunner,
	}})

	snap, _ := s.acquireSnapshot()
	if got := snap.MemoryRepoPath; got != "C:\\repo" {
		t.Fatalf("memory repo path = %q", got)
	}
	if snap.AgentRegistry != reg {
		t.Fatal("agent registry was not published")
	}
	if snap.MemoryStore != memStore {
		t.Fatal("memory store was not published")
	}
	if snap.SessionStore != sessionStore {
		t.Fatal("session store was not published")
	}
	if snap.Committer != committer {
		t.Fatal("committer was not published")
	}
	if snap.Dedup != dedup {
		t.Fatal("dedup checker was not published")
	}
	if snap.PromotionDedupThreshold != 0.95 {
		t.Fatalf("dedup threshold = %v", snap.PromotionDedupThreshold)
	}
	if snap.RetrievalScorer != scorer {
		t.Fatal("retrieval scorer was not published")
	}
	if snap.IndexRebuilder != rebuilder {
		t.Fatal("index rebuilder was not published")
	}
	if snap.ChatRunner != chatRunner {
		t.Fatal("chat runner was not published")
	}
	if snap.TaskRunner != taskRunner {
		t.Fatal("task runner was not published")
	}

	// A provider without a snapshot clears every generation-bound field
	// together, so handlers observe an empty snapshot.
	s.SetSnapshotProvider(&staticSnapshotProvider{})
	cleared, _ := s.acquireSnapshot()
	if cleared.MemoryRepoPath != "" || cleared.AgentRegistry != nil || cleared.MemoryStore != nil || cleared.SessionStore != nil || cleared.Committer != nil || cleared.Dedup != nil || cleared.RetrievalScorer != nil || cleared.IndexRebuilder != nil || cleared.ChatRunner != nil || cleared.TaskRunner != nil {
		t.Fatal("service deps were not cleared together")
	}
}

func TestSetSnapshotProviderNilYieldsEmptySnapshot(t *testing.T) {
	s := NewServer(3000)
	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		MemoryStore: newStubMemoryStore(nil),
		ChatRunner:  &stubChatRunner{},
	}})

	// A nil provider must clear the atomic pointer, not store a non-nil
	// pointer to a nil interface that acquisition would call into.
	s.SetSnapshotProvider(nil)

	snap, release := s.acquireSnapshot()
	defer release()
	if snap.MemoryStore != nil || snap.ChatRunner != nil {
		t.Fatalf("nil provider left deps visible: store=%T runner=%T", snap.MemoryStore, snap.ChatRunner)
	}
}
func TestNewBasePageUsesCachedProjectNav(t *testing.T) {
	s := NewServer(3000)
	store := &countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
	}}
	s.SetProjectStore(store)

	if got := atomic.LoadInt32(&store.listCalls); got != 1 {
		t.Fatalf("SetProjectStore should populate project nav once, got %d List calls", got)
	}

	s.state.mu.Lock()
	s.state.data.ProjectSlug = "demo"
	s.state.mu.Unlock()

	bp := s.newBasePage("status")
	if got := atomic.LoadInt32(&store.listCalls); got != 1 {
		t.Fatalf("newBasePage should use cached project nav, got %d List calls", got)
	}
	if bp.ActiveProjectSlug != "demo" || bp.ActiveProjectName != "Demo Project" {
		t.Fatalf("active project = %q/%q, want demo/Demo Project", bp.ActiveProjectSlug, bp.ActiveProjectName)
	}
	if len(bp.ProjectSlugs) != 2 || bp.ProjectSlugs[1] != "demo" {
		t.Fatalf("project slugs = %#v, want cached demo nav", bp.ProjectSlugs)
	}

	bp.ProjectNames["demo"] = "mutated"
	bp.ProjectSlugs[1] = "mutated"
	bp = s.newBasePage("status")
	if bp.ActiveProjectName != "Demo Project" || bp.ProjectSlugs[1] != "demo" {
		t.Fatalf("project nav snapshot was mutated across renders: %#v %#v", bp.ProjectNames, bp.ProjectSlugs)
	}
}

func TestListProjectsGETDoesNotRunProjectActions(t *testing.T) {
	s := NewServer(3000)
	store := &countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
	}}
	s.SetProjectStore(store)

	req := httptest.NewRequest(http.MethodGet, "/projects?activate=demo&hide=demo&unhide=demo", nil)
	rec := httptest.NewRecorder()
	s.listProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&store.hiddenCalls); got != 0 {
		t.Fatalf("GET /projects should not mutate visibility, got %d SetHidden calls", got)
	}
	if s.state.snapshot().ProjectSlug == "demo" {
		t.Fatal("GET /projects should not activate projects")
	}
}

func TestHandleProjectActivatePOST(t *testing.T) {
	s := NewServer(3000)
	cfgStore := &stubConfigStore{cfg: config.Defaults()}
	s.SetConfigStore(cfgStore)
	s.SetProjectStore(&countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
	}})

	form := url.Values{"slug": {"demo"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/activate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleProjectActivate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if cfgStore.cfg.Project.ActiveProjectSlug != "demo" {
		t.Fatalf("active project = %q, want demo", cfgStore.cfg.Project.ActiveProjectSlug)
	}
	if got := s.state.snapshot().ProjectSlug; got != "demo" {
		t.Fatalf("state project slug = %q, want demo", got)
	}
}

// The sidebar posts from any page, so after activation the user should be
// bounced back to the originating same-origin page instead of dumped on
// /projects. A foreign referrer must not be followed.
func TestHandleProjectActivateRedirectsToReferrer(t *testing.T) {
	s := NewServer(3000)
	cfgStore := &stubConfigStore{cfg: config.Defaults()}
	s.SetConfigStore(cfgStore)
	s.SetProjectStore(&countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
	}})

	form := url.Values{"slug": {"demo"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/activate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:3000"
	req.Header.Set("Referer", "http://127.0.0.1:3000/status?q=1")
	rec := httptest.NewRecorder()
	s.handleProjectActivate(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://127.0.0.1:3000/status") {
		t.Fatalf("Location = %q, want bounce back to referrer", loc)
	}
	if !strings.Contains(loc, "flash=") {
		t.Errorf("expected flash to be appended to the referrer redirect, got %q", loc)
	}
}

func TestHandleProjectActivateRejectsForeignReferrer(t *testing.T) {
	s := NewServer(3000)
	cfgStore := &stubConfigStore{cfg: config.Defaults()}
	s.SetConfigStore(cfgStore)
	s.SetProjectStore(&countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
	}})

	form := url.Values{"slug": {"demo"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/activate", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://evil.example/")
	rec := httptest.NewRecorder()
	s.handleProjectActivate(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/projects?flash=") {
		t.Fatalf("Location = %q, want fallback to /projects", loc)
	}
}

func TestHandleProjectActionsRequirePOST(t *testing.T) {
	s := NewServer(3000)

	for name, handler := range map[string]http.HandlerFunc{
		"activate": s.handleProjectActivate,
		"hide":     s.handleProjectHide,
		"unhide":   s.handleProjectUnhide,
	} {
		req := httptest.NewRequest(http.MethodGet, "/projects/"+name+"?slug=demo", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: expected 405, got %d", name, rec.Code)
		}
	}
}

func TestHandleProjectVisibilityPOST(t *testing.T) {
	s := NewServer(3000)
	store := &countingProjectStore{projects: []project.Project{{Slug: "demo", DisplayName: "Demo Project"}}}
	s.SetProjectStore(store)

	form := url.Values{"slug": {"demo"}}
	req := httptest.NewRequest(http.MethodPost, "/projects/hide", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleProjectHide(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("hide: expected 303, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&store.hiddenCalls); got != 1 || store.hiddenSlug != "demo" || !store.hiddenValue {
		t.Fatalf("hide SetHidden = calls:%d slug:%q hidden:%v", got, store.hiddenSlug, store.hiddenValue)
	}

	req = httptest.NewRequest(http.MethodPost, "/projects/unhide", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.handleProjectUnhide(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unhide: expected 303, got %d", rec.Code)
	}
	if got := atomic.LoadInt32(&store.hiddenCalls); got != 2 || store.hiddenSlug != "demo" || store.hiddenValue {
		t.Fatalf("unhide SetHidden = calls:%d slug:%q hidden:%v", got, store.hiddenSlug, store.hiddenValue)
	}
}
func TestHandleStatus_OK(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Harness") {
		t.Error("expected 'Harness' in response body")
	}
}

type recordingTaskRunner struct {
	cancelSession string
	cancelErr     error
	agent         string
	sessionID     string
	conversation  []ChatMessage
	ran           chan struct{}
}

func (r *recordingTaskRunner) RunTask(_ context.Context, agent, sessionID string, conversation []ChatMessage) (string, <-chan TaskEvent, error) {
	r.agent = agent
	r.sessionID = sessionID
	r.conversation = append([]ChatMessage(nil), conversation...)
	if r.ran != nil {
		close(r.ran)
	}
	ch := make(chan TaskEvent)
	close(ch)
	return "", ch, nil
}

func (r *recordingTaskRunner) CancelTask(sessionID string) error {
	r.cancelSession = sessionID
	return r.cancelErr
}

func (r *recordingTaskRunner) ApplyApproval(string, string, string) error {
	return nil
}

func TestBroadcastTaskSSEReliableFramesWaitForSubscriber(t *testing.T) {
	s := NewServer(3000)
	ch := make(chan string, 1)
	ch <- "existing"
	s.taskSSEClients.Store(ch, "task-a")
	defer s.taskSSEClients.Delete(ch)

	done := make(chan struct{})
	go func() {
		s.broadcastTaskSSE("task-a", "event: task-event\ndata: final\n\n")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("reliable task frame was dropped instead of waiting for delivery")
	case <-time.After(50 * time.Millisecond):
	}

	if got := <-ch; got != "existing" {
		t.Fatalf("first frame = %q, want prefilled frame", got)
	}

	select {
	case got := <-ch:
		if !strings.Contains(got, "final") {
			t.Fatalf("reliable frame = %q, want final payload", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reliable task frame")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast did not complete after subscriber drained")
	}
}

func TestBroadcastTaskSSETextFramesWaitForSubscriber(t *testing.T) {
	s := NewServer(3000)
	ch := make(chan string, 1)
	ch <- "existing"
	s.taskSSEClients.Store(ch, "task-a")
	defer s.taskSSEClients.Delete(ch)

	done := make(chan struct{})
	go func() {
		s.broadcastTaskSSE("task-a", "event: task-text\ndata: token\n\n")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("task text frame was dropped instead of waiting for delivery")
	case <-time.After(50 * time.Millisecond):
	}
	if got := <-ch; got != "existing" {
		t.Fatalf("first frame = %q, want prefilled frame", got)
	}
	select {
	case got := <-ch:
		if !strings.Contains(got, "token") {
			t.Fatalf("text frame = %q, want token payload", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task text frame")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast did not complete after subscriber drained")
	}
}
func TestBroadcastTaskSSERoutesByStreamID(t *testing.T) {
	s := NewServer(3000)
	wantCh := make(chan string, 1)
	otherCh := make(chan string, 1)
	s.taskSSEClients.Store(wantCh, "task-a")
	s.taskSSEClients.Store(otherCh, "task-b")
	defer s.taskSSEClients.Delete(wantCh)
	defer s.taskSSEClients.Delete(otherCh)

	s.broadcastTaskSSE("task-a", "event: task-event\ndata: final\n\n")

	select {
	case got := <-wantCh:
		if !strings.Contains(got, "final") {
			t.Fatalf("routed frame = %q, want final", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for routed task frame")
	}
	select {
	case got := <-otherCh:
		t.Fatalf("other stream received frame %q", got)
	default:
	}
}

func TestHandleTaskSendUsesLiveConversationForResume(t *testing.T) {
	s := NewServer(3000)
	runner := &recordingTaskRunner{ran: make(chan struct{})}
	store := &stubSessionStore{liveResult: []ChatMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
	}}
	setTaskRunnerForTest(s, runner)
	setSessionStoreForTest(s, store)

	form := url.Values{
		"agent":      {"coder"},
		"session_id": {"task-123"},
		"stream_id":  {"stream-1"},
		"message":    {"follow-up"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskSend(rec, req)
	select {
	case <-runner.ran:
	case <-time.After(time.Second):
		t.Fatal("runner was not invoked")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `sse-swap="task-text-`) {
		t.Fatalf("task response missing turn-specific text SSE event: %s", body)
	}
	if strings.Contains(body, `sse-swap="task-text"`) {
		t.Fatalf("task response kept generic task-text SSE event: %s", body)
	}
	if runner.sessionID != "task-123" || runner.agent != "coder" {
		t.Fatalf("runner got agent/session = %q/%q", runner.agent, runner.sessionID)
	}
	want := []ChatMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "follow-up"},
	}
	if len(runner.conversation) != len(want) {
		t.Fatalf("conversation length = %d, want %d: %+v", len(runner.conversation), len(want), runner.conversation)
	}
	for i := range want {
		if runner.conversation[i] != want[i] {
			t.Fatalf("conversation[%d] = %+v, want %+v", i, runner.conversation[i], want[i])
		}
	}
}

func TestHandleTaskCancel_Success(t *testing.T) {
	s := NewServer(3000)
	runner := &recordingTaskRunner{}
	setTaskRunnerForTest(s, runner)

	form := url.Values{"session_id": {"task-123"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskCancel(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if runner.cancelSession != "task-123" {
		t.Fatalf("cancel session = %q, want task-123", runner.cancelSession)
	}
}

func TestHandleTaskCancel_Validation(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	s.handleTaskCancel(rec, httptest.NewRequest(http.MethodGet, "/task/cancel", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/cancel", strings.NewReader("session_id=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskCancel(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without runner status = %d, want 503", rec.Code)
	}

	setTaskRunnerForTest(s, &recordingTaskRunner{})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/task/cancel", strings.NewReader("session_id="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskCancel(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing session status = %d, want 400", rec.Code)
	}
}

func TestHandleTaskCancel_RunnerError(t *testing.T) {
	s := NewServer(3000)
	setTaskRunnerForTest(s, &recordingTaskRunner{cancelErr: errors.New("no active task")})

	form := url.Values{"session_id": {"missing"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/task/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleTaskCancel(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no active task") {
		t.Fatalf("body missing runner error: %s", rec.Body.String())
	}
}
func TestHandleStatus_WithErrors(t *testing.T) {
	s := NewServer(3000)
	s.AddStartupError(errors.New("llama-server binary not found: C:\\missing.exe"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "llama-server binary not found") {
		t.Error("expected startup error in response body")
	}
}

func TestHandleStatus_ProjectDirectoryWarnings(t *testing.T) {
	s := NewServer(3000)
	path := filepath.Join(t.TempDir(), "missing-repo")
	s.SetProjectDirectoryWarnings("dt", []ProjectDirectoryWarning{{Path: path, Problem: "directory missing"}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	for _, want := range []string{"Project directory issues", "dt", path, "directory missing", "keep running"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q in response body", want)
		}
	}
}

func TestHandleStatus_FirstRunShowsSetupCTA(t *testing.T) {
	s := NewServer(3000)
	s.SetFirstRun(true)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "Set up your harness") {
		t.Error("expected first-run CTA when FirstRun=true")
	}
	if !strings.Contains(string(body), "/config") {
		t.Error("expected CTA to link to /config")
	}
}

func TestSetLlamaStatus(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama", Running: true, Healthy: true})

	if !s.state.snapshot().LlamaStatus.Healthy {
		t.Error("expected llama status healthy")
	}
}

func TestSetQueueDepth(t *testing.T) {
	s := NewServer(3000)
	s.SetQueueDepth(3, 8)

	snap := s.state.snapshot()
	if snap.QueueDepth != 3 || snap.QueueMax != 8 {
		t.Errorf("expected depth 3/8, got %d/%d", snap.QueueDepth, snap.QueueMax)
	}
}

func TestHandleConfig_GETRendersFormWithDefaults(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="model_binary"`) {
		t.Error("expected form field for model binary")
	}
	if strings.Contains(body, "`r`n") {
		t.Error("config page must not render a literal escaped newline")
	}
	if !strings.Contains(body, "First run") {
		t.Error("expected first-run banner when config has never been saved")
	}
}

func TestHandleConfig_GETWithoutStoreShowsError(t *testing.T) {
	s := NewServer(3000) // no store attached

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "config store unavailable") {
		t.Error("expected 'config store unavailable' message when no store is attached")
	}
}

func TestHandleConfig_POSTSavesAndRedirects(t *testing.T) {
	s, store := newServerWithStore(t)

	var retryCalls int32
	s.SetRetry(func() ApplyResult { atomic.AddInt32(&retryCalls, 1); return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_ctx_size", "8192")
	form.Set("model_gpu_layers", "20")
	form.Set("model_n_parallel", "1")
	form.Set("model_port", "8081")
	form.Set("model_verbose", "on")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("embed_verbose", "on")
	form.Set("ui_port", "3000")
	form.Set("ui_open_on_start", "on")
	form.Set("api_port", "8080")
	form.Set("project_llama_on_switch", "keep")
	form.Set("prompt_memory_budget", "2048")
	form.Set("prompt_conversation_reserve", "4096")
	form.Set("prompt_recency_n", "7")
	form.Set("prompt_semantic_weight", "0.71")
	form.Set("prompt_recency_weight", "0.29")
	form.Set("prompt_promotion_dedup_threshold", "0.83")
	form.Set("prompt_summarizer_prompt", "summarize the user's intent in one paragraph.")
	form.Set("queue_max_depth", "8")
	form.Set("loop_max_turns", "12")
	form.Set("loop_doom_threshold", "4")
	form.Set("loop_read_enabled", "on")
	form.Set("loop_web_search_enabled", "on")
	form.Set("metrics_retention_days", "30")
	form.Set("metrics_prometheus_enabled", "on")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/config?saved=1" {
		t.Errorf("expected redirect to /config?saved=1, got %q", got)
	}

	loaded, configured, err := store.Load()
	if err != nil {
		t.Fatalf("loading saved config failed: %v", err)
	}
	if !configured {
		t.Error("expected configured=true after POST")
	}
	if loaded.Model.Binary != "C:\\llama.exe" {
		t.Errorf("model binary not persisted: got %q", loaded.Model.Binary)
	}
	if !loaded.Model.Verbose {
		t.Error("expected Model.Verbose=true after POST with model_verbose=on")
	}
	if !loaded.Embedder.Verbose {
		t.Error("expected Embedder.Verbose=true after POST with embed_verbose=on")
	}
	if loaded.Project.LlamaOnSwitch != "keep" {
		t.Errorf("Project.LlamaOnSwitch not persisted: got %q, want keep", loaded.Project.LlamaOnSwitch)
	}
	if loaded.Prompt.RecencyN != 7 {
		t.Errorf("Prompt.RecencyN not persisted: got %d, want 7", loaded.Prompt.RecencyN)
	}
	if loaded.Prompt.SummarizerPrompt != "summarize the user's intent in one paragraph." {
		t.Errorf("Prompt.SummarizerPrompt not persisted: got %q", loaded.Prompt.SummarizerPrompt)
	}
	if loaded.Prompt.SemanticWeight != 0.71 {
		t.Errorf("Prompt.SemanticWeight not persisted: got %v, want 0.71", loaded.Prompt.SemanticWeight)
	}
	if loaded.Prompt.RecencyWeight != 0.29 {
		t.Errorf("Prompt.RecencyWeight not persisted: got %v, want 0.29", loaded.Prompt.RecencyWeight)
	}
	if loaded.Prompt.PromotionDedupThreshold != 0.83 {
		t.Errorf("Prompt.PromotionDedupThreshold not persisted: got %v, want 0.83", loaded.Prompt.PromotionDedupThreshold)
	}
	if loaded.Loop.MaxTurns != 12 {
		t.Errorf("Loop.MaxTurns not persisted: got %d, want 12", loaded.Loop.MaxTurns)
	}
	if loaded.Loop.DoomThreshold != 4 {
		t.Errorf("Loop.DoomThreshold not persisted: got %d, want 4", loaded.Loop.DoomThreshold)
	}
	if !loaded.Loop.ReadEnabled {
		t.Error("expected Loop.ReadEnabled=true after POST with loop_read_enabled=on")
	}
	if loaded.Loop.FileListEnabled {
		t.Error("expected Loop.FileListEnabled=false after POST without loop_file_list_enabled")
	}
	if !loaded.Loop.WebSearchEnabled {
		t.Error("expected Loop.WebSearchEnabled=true after POST with loop_web_search_enabled=on")
	}

	if atomic.LoadInt32(&retryCalls) != 1 {
		t.Errorf("expected retry callback to fire once, got %d", retryCalls)
	}
}

// A subsequent POST without the verbose checkboxes must clear them - HTML
// forms omit unchecked checkboxes entirely, so missing value means false.
func TestHandleConfig_POSTClearsVerboseWhenUnchecked(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	// Seed with verbose=true.
	seed := config.Defaults()
	seed.Model.Binary = "C:\\llama.exe"
	seed.Model.ModelPath = "C:\\m.gguf"
	seed.Model.Verbose = true
	seed.Embedder.Binary = "C:\\embed.exe"
	seed.Embedder.ModelPath = "C:\\e.gguf"
	seed.Embedder.Verbose = true
	if err := store.Save(&seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_port", "8081")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")
	// Deliberately omit model_verbose and embed_verbose.

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Model.Verbose {
		t.Error("expected Model.Verbose=false when form omitted the checkbox")
	}
	if loaded.Embedder.Verbose {
		t.Error("expected Embedder.Verbose=false when form omitted the checkbox")
	}
}

// Windows "Copy as path" wraps the path in double quotes; users paste that
// verbatim. The form parser must strip the surrounding quotes so the stored
// value is the raw path.
func TestHandleConfig_POSTStripsQuotedPaths(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", `"C:\llama.exe"`)
	form.Set("model_path", `"C:\m.gguf"`)
	form.Set("model_port", "8081")
	form.Set("embed_binary", `'C:\embed.exe'`)
	form.Set("embed_path", `'C:\e.gguf'`)
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, got := range map[string]string{
		"Model.Binary":       loaded.Model.Binary,
		"Model.ModelPath":    loaded.Model.ModelPath,
		"Embedder.Binary":    loaded.Embedder.Binary,
		"Embedder.ModelPath": loaded.Embedder.ModelPath,
	} {
		if strings.HasPrefix(got, `"`) || strings.HasPrefix(got, "'") {
			t.Errorf("%s: expected unquoted path, got %q", name, got)
		}
	}
	if loaded.Model.Binary != `C:\llama.exe` {
		t.Errorf("Model.Binary = %q, want C:\\llama.exe", loaded.Model.Binary)
	}
	if loaded.Model.ModelPath != `C:\m.gguf` {
		t.Errorf("Model.ModelPath = %q, want C:\\m.gguf", loaded.Model.ModelPath)
	}
	if loaded.Embedder.Binary != `C:\embed.exe` {
		t.Errorf("Embedder.Binary = %q, want C:\\embed.exe", loaded.Embedder.Binary)
	}
	if loaded.Embedder.ModelPath != `C:\e.gguf` {
		t.Errorf("Embedder.ModelPath = %q, want C:\\e.gguf", loaded.Embedder.ModelPath)
	}
}

func TestTrimPathField(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"D:\tmp\models\foo.gguf"`, `D:\tmp\models\foo.gguf`},
		{`'D:\tmp\models\foo.gguf'`, `D:\tmp\models\foo.gguf`},
		{`  "D:\foo.gguf"  `, `D:\foo.gguf`},
		{`C:\foo.gguf`, `C:\foo.gguf`},
		{`"C:\foo"bar`, `"C:\foo"bar`},
		{`"`, `"`},
		{``, ``},
	}
	for _, c := range cases {
		if got := trimPathField(c.in); got != c.want {
			t.Errorf("trimPathField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHandleConfig_POSTIncludesApplyResultInRedirect(t *testing.T) {
	s, _ := newServerWithStore(t)
	s.SetRetry(func() ApplyResult {
		return ApplyResult{
			LiveApplied:   true,
			RestartNeeded: []string{"UI port", "queue max depth"},
		}
	})

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("model_port", "8081")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("embed_port", "8082")
	form.Set("ui_port", "3000")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/config?saved=1&applied=1&restart=UI+port%7Cqueue+max+depth"
	if loc != want {
		t.Errorf("redirect mismatch:\n got: %q\nwant: %q", loc, want)
	}
}

func TestHandleConfig_GETParsesApplyResultFromQuery(t *testing.T) {
	s, store := newServerWithStore(t)
	// Pre-seed so renderConfig has something to render.
	_ = store.Save(&config.Config{
		Model:    config.ModelConfig{Binary: "x", ModelPath: "y", Port: 1},
		Embedder: config.EmbedderConfig{Binary: "x", ModelPath: "y", Port: 2},
		UI:       config.UIConfig{Port: 3},
	})

	req := httptest.NewRequest(http.MethodGet, "/config?saved=1&applied=1&restart=UI+port%7Cqueue+max+depth", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Model and embedder are reloading live") {
		t.Errorf("expected mixed-apply message in body, got:\n%s", body)
	}
	if !strings.Contains(body, "UI port") || !strings.Contains(body, "queue max depth") {
		t.Errorf("expected restart reasons in body, got:\n%s", body)
	}
}

func TestHandleConfig_POSTRejectsInvalidNumericInput(t *testing.T) {
	s, store := newServerWithStore(t)

	existing := config.Defaults()
	existing.Model.Binary = "C:\\existing.exe"
	existing.Model.ModelPath = "C:\\existing.gguf"
	existing.Embedder.Binary = "C:\\embed.exe"
	existing.Embedder.ModelPath = "C:\\embed.gguf"
	existing.Model.CtxSize = 11111
	if err := store.Save(&existing); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	form := url.Values{}
	form.Set("model_binary", existing.Model.Binary)
	form.Set("model_path", existing.Model.ModelPath)
	form.Set("embed_binary", existing.Embedder.Binary)
	form.Set("embed_path", existing.Embedder.ModelPath)
	form.Set("model_ctx_size", "not-a-number")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected form re-render 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Model context size must be an integer") {
		t.Fatalf("expected field parse error, got body:\n%s", rec.Body.String())
	}
	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("load after rejected save: %v", err)
	}
	if loaded.Model.CtxSize != 11111 {
		t.Fatalf("invalid numeric input was saved: ctx size = %d", loaded.Model.CtxSize)
	}
}
func TestHandleConfig_POSTPreservesExistingNumericsWhenBlank(t *testing.T) {
	s, store := newServerWithStore(t)

	// Seed store with a config whose numeric values diverge from Defaults.
	existing := config.Defaults()
	existing.Model.Binary = "C:\\existing.exe"
	existing.Model.ModelPath = "C:\\existing.gguf"
	existing.Model.CtxSize = 20000
	existing.Model.GPULayers = 42
	existing.Embedder.Binary = "C:\\eb.exe"
	existing.Embedder.ModelPath = "C:\\eb.gguf"
	existing.Prompt.MemoryTokenBudget = 9999
	if err := store.Save(&existing); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	form := url.Values{}
	// Update only the string fields; leave every numeric field blank.
	form.Set("model_binary", "C:\\new.exe")
	form.Set("model_path", "C:\\new.gguf")
	form.Set("embed_binary", "C:\\eb.exe")
	form.Set("embed_path", "C:\\eb.gguf")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}

	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if loaded.Model.CtxSize != 20000 {
		t.Errorf("expected Model.CtxSize preserved (20000), got %d", loaded.Model.CtxSize)
	}
	if loaded.Model.GPULayers != 42 {
		t.Errorf("expected Model.GPULayers preserved (42), got %d", loaded.Model.GPULayers)
	}
	if loaded.Prompt.MemoryTokenBudget != 9999 {
		t.Errorf("expected Prompt.MemoryTokenBudget preserved (9999), got %d", loaded.Prompt.MemoryTokenBudget)
	}
	if loaded.Model.Binary != "C:\\new.exe" {
		t.Errorf("expected Model.Binary updated, got %q", loaded.Model.Binary)
	}
}

func TestHandleConfig_POSTPersistsLogBufferFields(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("log_ring_max_entries", "1500")
	form.Set("log_proc_max_lines", "200")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Log.RingMaxEntries != 1500 {
		t.Errorf("RingMaxEntries: got %d, want 1500", loaded.Log.RingMaxEntries)
	}
	if loaded.Log.ProcMaxLines != 200 {
		t.Errorf("ProcMaxLines: got %d, want 200", loaded.Log.ProcMaxLines)
	}
}

func TestHandleConfig_GETRendersWebSearchToggle(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, want := range []string{`name="loop_web_search_enabled"`, `Enable <code>web_search</code>`, `sends the query over the network`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected config form to include %q", want)
		}
	}
}

func TestHandleConfig_GETRendersLogFields(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, want := range []string{`name="log_ring_max_entries"`, `name="log_proc_max_lines"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected config form to include %q", want)
		}
	}
}

func TestHandleConfig_GETRendersPromotionDedupThreshold(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, want := range []string{`name="prompt_promotion_dedup_threshold"`, `Promotion dedup threshold`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected config form to include %q", want)
		}
	}
}

func TestHandleConfig_GETRendersCacheTypeSelects(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`name="model_cache_type_k"`,
		`name="model_cache_type_v"`,
		`<option value="q8_0" selected>q8_0</option>`,
		`<option value="f16"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected config form to include %q", want)
		}
	}
}

func TestHandleConfig_POSTPersistsCacheTypes(t *testing.T) {
	s, store := newServerWithStore(t)
	s.SetRetry(func() ApplyResult { return ApplyResult{} })

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("model_cache_type_k", "q4_0")
	form.Set("model_cache_type_v", "f16")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	loaded, _, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Model.CacheTypeK != "q4_0" {
		t.Errorf("CacheTypeK: got %q, want q4_0", loaded.Model.CacheTypeK)
	}
	if loaded.Model.CacheTypeV != "f16" {
		t.Errorf("CacheTypeV: got %q, want f16", loaded.Model.CacheTypeV)
	}
}

func TestHandleConfig_POSTRejectsUnknownCacheType(t *testing.T) {
	s, _ := newServerWithStore(t)

	form := url.Values{}
	form.Set("model_binary", "C:\\llama.exe")
	form.Set("model_path", "C:\\m.gguf")
	form.Set("embed_binary", "C:\\embed.exe")
	form.Set("embed_path", "C:\\e.gguf")
	form.Set("model_cache_type_k", "q3_k")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render with error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model.cache_type_k must be one of") {
		t.Errorf("expected cache_type_k validation error in body, got: %s", rec.Body.String())
	}
}

func TestHandleConfig_POSTInvalidShowsValidationError(t *testing.T) {
	s, store := newServerWithStore(t)

	form := url.Values{}
	// Deliberately omit model_binary, model_path, embed_binary, embed_path.
	form.Set("ui_port", "3000")
	form.Set("model_port", "8081")
	form.Set("embed_port", "8082")

	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render with error), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Validation error") {
		t.Error("expected validation error message in rendered form")
	}

	_, configured, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if configured {
		t.Error("config should not be marked configured when validation fails")
	}
}

func TestHandleConfig_GETRendersDatalistAnchors(t *testing.T) {
	s, _ := newServerWithStore(t)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	for _, id := range []string{
		"model_binary_options",
		"model_path_options",
		"embed_binary_options",
		"embed_path_options",
	} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("expected datalist with id=%q", id)
		}
	}
}

func TestHandleConfig_GETPreFillsDetectedLlamaBinary(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}
	want := filepath.Join(dir, exe)
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.SetBinDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `name="model_binary" value="`+want+`"`) {
		t.Errorf("expected detected llama-server %q to pre-fill model_binary", want)
	}
	// The embedder runs the same llama-server binary in --embedding mode, so
	// it defaults to the same resolved path when left blank.
	if !strings.Contains(body, `name="embed_binary" value="`+want+`"`) {
		t.Errorf("expected detected llama-server %q to pre-fill embed_binary", want)
	}
}

func TestHandleConfig_GETOffersModelSuggestionsInDatalist(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.Mkdir(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir models: %v", err)
	}
	main := filepath.Join(modelsDir, "Qwen3-35B.gguf")
	embed := filepath.Join(modelsDir, "nomic-embed-v2.gguf")
	for _, p := range []string{main, embed} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	s.SetBinDir(dir)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `value="`+main+`"`) {
		t.Errorf("expected main model option for %s in rendered body", main)
	}
	if !strings.Contains(body, `value="`+embed+`"`) {
		t.Errorf("expected embedder model option for %s in rendered body", embed)
	}
}

func TestHandleConfig_POSTErrorDoesNotPreFillBinary(t *testing.T) {
	s, _ := newServerWithStore(t)

	dir := t.TempDir()
	exe := "llama-server"
	if runtime.GOOS == "windows" {
		exe = "llama-server.exe"
	}
	detected := filepath.Join(dir, exe)
	if err := os.WriteFile(detected, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.SetBinDir(dir)

	// POST with blank required fields → re-renders with ValidationErr. The
	// rendered form should echo the user's submission (empty) rather than
	// silently inserting the detected path.
	form := url.Values{}
	form.Set("ui_port", "3000")
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `name="model_binary" value="`+detected+`"`) {
		t.Error("POST error re-render should not overwrite the user's submitted value with detected path")
	}
}

func TestHandleRetry_CallsCallback(t *testing.T) {
	s := NewServer(3000)
	var called int32
	s.SetRetry(func() ApplyResult { atomic.AddInt32(&called, 1); return ApplyResult{} })

	req := httptest.NewRequest(http.MethodPost, "/retry", nil)
	rec := httptest.NewRecorder()
	s.handleRetry(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Errorf("expected retry to be called once, got %d", called)
	}
}

func TestHandleRetry_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/retry", nil)
	rec := httptest.NewRecorder()
	s.handleRetry(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleProcRestart_CallsMatchingCallback(t *testing.T) {
	s := NewServer(3000)
	var llama, embed int32
	s.SetProcRestarts(
		func() { atomic.AddInt32(&llama, 1) },
		func() { atomic.AddInt32(&embed, 1) },
	)

	req := httptest.NewRequest(http.MethodPost, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("llama: expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&llama) != 1 || atomic.LoadInt32(&embed) != 0 {
		t.Errorf("expected llama callback only, llama=%d embed=%d", llama, embed)
	}

	req = httptest.NewRequest(http.MethodPost, "/procs/embed/restart", nil)
	rec = httptest.NewRecorder()
	s.handleProcRestart("embed")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("embed: expected 303, got %d", rec.Code)
	}
	if atomic.LoadInt32(&embed) != 1 {
		t.Errorf("expected embed callback to be called, got %d", embed)
	}
}

func TestHandleProcRestart_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleProcRestart_NoCallbackStillRedirects(t *testing.T) {
	// The manager may not be up yet on first run. The handler must not
	// panic and must still redirect so the UI doesn't show a blank page.
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodPost, "/procs/llama/restart", nil)
	rec := httptest.NewRecorder()
	s.handleProcRestart("llama")(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected 303 even without callback, got %d", rec.Code)
	}
}

func TestHandleStatus_RendersRestartFormWhenFailed(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama-server", Failed: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `action="/procs/llama/restart"`) {
		t.Error("status page should include llama restart form when Failed")
	}
	// The restart form must be visible (no hidden attribute) when Failed.
	if !strings.Contains(body, `id="llama-restart-form"`) {
		t.Fatal("llama restart form missing from body")
	}
	if strings.Contains(body, `id="llama-restart-form" hidden`) ||
		strings.Contains(body, `hidden id="llama-restart-form"`) {
		t.Error("llama restart form should not be hidden when Failed=true")
	}
	// Status text can have surrounding whitespace from the template; just
	// verify the word appears in the status panel and the other states do not.
	const open = `id="llama-status-panel"`
	i := strings.Index(body, open)
	j := strings.Index(body[i:], `id="llama-restart-form"`)
	if i < 0 || j < 0 {
		t.Fatal("could not locate llama status panel in rendered body")
	}
	panelHead := body[i : i+j]
	if !strings.Contains(panelHead, "Failed") {
		t.Error("status panel should read 'Failed' when Status.Failed is true")
	}
	if strings.Contains(panelHead, "Healthy") || strings.Contains(panelHead, "Unhealthy") {
		t.Errorf("status panel should not render Healthy/Unhealthy when Failed; got %q", panelHead)
	}
}

func TestHandleStatus_HidesRestartFormWhenNotFailed(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Name: "llama-server", Running: true, Healthy: true})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `id="llama-restart-form"`) {
		t.Fatal("llama restart form should still be in DOM")
	}
	if !strings.Contains(body, `hidden`) {
		t.Error("llama restart form should be hidden when not Failed")
	}
}

func TestHandleStatus_RendersRecentLogs(t *testing.T) {
	s := NewServer(3000)
	ring := logbuf.New(10)
	s.SetLogRing(ring)
	if _, err := ring.Write([]byte("hello world\nsecond line\n")); err != nil {
		t.Fatalf("ring write: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"hello world", "second line"} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing log line %q", want)
		}
	}
}

func TestHandleStatus_NoLogRingRendersEmpty(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// flushRecorder is a ResponseRecorder that satisfies http.Flusher so streaming
// handlers don't bail out on the type assertion. Flush is a no-op because the
// recorder always has the bytes available immediately.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

// runSSE drives handleSSE in a goroutine, lets the caller publish, and tears
// down via context cancel. Returns the captured response body. The 50 ms
// pauses on either side of publish are enough for the subscription to
// register and for the fan-out + write to land on the recorder.
func runSSE(t *testing.T, s *Server, publish func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		s.handleSSE(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if publish != nil {
		publish()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}
	return rec.Body.String()
}

func TestSSE_EmitsConnectedAndInitialState(t *testing.T) {
	s := NewServer(3000)
	s.SetLlamaStatus(ProcessStatus{Running: true, Healthy: true})

	body := runSSE(t, s, nil)

	if !strings.HasPrefix(body, ": connected\n\n") {
		t.Errorf("SSE payload did not begin with connected comment, got: %q", body)
	}
	if !strings.Contains(body, "event: state\n") {
		t.Errorf("SSE payload missing initial state event, got: %q", body)
	}
	// OOB HTML fragments replace the old JSON payload.
	for _, want := range []string{
		`id="llama-status-panel" hx-swap-oob="true"`,
		`id="embed-status-panel" hx-swap-oob="true"`,
		`id="queue-card" hx-swap-oob="true"`,
		`id="uptime" hx-swap-oob="true"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE payload missing OOB fragment %q, got: %q", want, body)
		}
	}
}

func TestSSE_EmitsProjectDirectoryWarnings(t *testing.T) {
	s := NewServer(3000)
	s.SetProjectDirectoryWarnings("dt", []ProjectDirectoryWarning{{Path: "/tmp/missing", Problem: "directory missing"}})

	body := runSSE(t, s, nil)

	// Project directory warnings render server-side in the initial page
	// template, not as live SSE data. The state event carries OOB HTML
	// fragments for process panels, queue, and uptime — not raw project
	// metadata. Verify the state event is still emitted correctly.
	if !strings.Contains(body, "event: state\n") {
		t.Errorf("SSE payload missing state event, got: %q", body)
	}
}

func TestSSE_EmitsHarnessLogEntries(t *testing.T) {
	s := NewServer(3000)
	ring := logbuf.New(10)
	s.SetLogRing(ring)

	body := runSSE(t, s, func() {
		if _, err := ring.Write([]byte("hello sse\n")); err != nil {
			t.Fatalf("ring write: %v", err)
		}
	})

	if !strings.Contains(body, "event: harness-log\n") {
		t.Errorf("SSE payload missing harness-log event, got: %q", body)
	}
	if !strings.Contains(body, "hello sse") {
		t.Errorf("SSE payload missing line, got: %q", body)
	}
}

func TestSSE_EmitsLlamaAndEmbedLogEntries(t *testing.T) {
	s := NewServer(3000)
	llama := logbuf.New(10)
	embed := logbuf.New(10)
	s.SetLlamaOutputRing(llama)
	s.SetEmbedOutputRing(embed)

	body := runSSE(t, s, func() {
		if _, err := llama.Write([]byte("from llama\n")); err != nil {
			t.Fatalf("llama write: %v", err)
		}
		if _, err := embed.Write([]byte("from embed\n")); err != nil {
			t.Fatalf("embed write: %v", err)
		}
	})

	for _, want := range []string{"event: llama-log\n", "from llama", "event: embed-log\n", "from embed"} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE payload missing %q, got: %q", want, body)
		}
	}
}

func TestSSE_NoRingsStillStreamsState(t *testing.T) {
	// No log rings wired - the connection should still open and emit state.
	s := NewServer(3000)

	body := runSSE(t, s, nil)

	if !strings.HasPrefix(body, ": connected\n\n") {
		t.Errorf("SSE payload did not begin with connected comment, got: %q", body)
	}
	if !strings.Contains(body, "event: state\n") {
		t.Errorf("SSE payload missing state event when no rings are wired, got: %q", body)
	}
}

func TestStart_ServerStarts(t *testing.T) {
	s := NewServer(13001) // use a high port to avoid conflicts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	var resp *http.Response
	var err error
	for i := 0; i < 10; i++ {
		resp, err = http.Get("http://localhost:13001/")
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("could not connect to UI server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// The management UI is unauthenticated and exposes state-changing routes, so
// it must never listen on a routable interface. originPolicy only stops
// cross-origin browsers; a non-browser client omitting Origin walks straight
// through. The bind address is the control that actually holds.
func TestStart_BindsLoopbackOnly(t *testing.T) {
	const port = 13002
	s := NewServer(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// Confirm it is actually up on loopback before asserting the negative.
	var up bool
	for i := 0; i < 20; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			_ = c.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatalf("server never came up on loopback:%d", port)
	}

	for _, ip := range nonLoopbackIPv4(t) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), time.Second)
		if err == nil {
			_ = conn.Close()
			t.Errorf("UI server is reachable on routable address %s:%d; it must bind loopback only", ip, port)
		}
	}
}

// nonLoopbackIPv4 returns the host's routable IPv4 addresses, skipping the
// test if there are none (isolated build environments).
func nonLoopbackIPv4(t *testing.T) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	var out []string
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil {
			continue
		}
		out = append(out, n.IP.String())
	}
	if len(out) == 0 {
		t.Skip("no routable IPv4 interface available")
	}
	return out
}

// The layout carries a persistent left sidebar listing every project. Each
// row has an activate button and a gear linking to that project's config
// (the per-project edit form, or /config for the reserved global project).
func TestLayout_RendersProjectSidebar(t *testing.T) {
	s := NewServer(3000)
	cfgStore := &stubConfigStore{cfg: config.Defaults()}
	s.SetConfigStore(cfgStore)
	s.SetProjectStore(&countingProjectStore{projects: []project.Project{
		{Slug: project.GlobalSlug, DisplayName: "Global"},
		{Slug: "demo", DisplayName: "Demo Project"},
		{Slug: "docs", DisplayName: "Docs"},
	}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`class="sidebar-projects"`,
		"Demo Project",
		`href="/projects?edit=demo"`,
		`href="/projects?edit=docs"`,
		`class="sidebar-gear" title="Configure global settings"`,
		`class="sidebar-project active"`,
		`class="sidebar-add" title="Add project"`,
		`onclick="document.getElementById('project-create-modal').showModal()"`,
		`id="project-create-modal"`,
		`id="sidebar-toggle"`,
		`aria-controls="project-sidebar"`,
		`class="sidebar-bottom-link`,
		`class="inline-confirm-panel sidebar-shutdown shutdown-confirm-panel"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered layout is missing sidebar element %q", want)
		}
	}
	if strings.Contains(body, `class="project-switcher-form"`) {
		t.Error("top-bar project switcher should be gone; activation happens via the sidebar")
	}
	if strings.Contains(body, `class="topbar-actions"`) {
		t.Error("top bar should no longer carry actions (Status/Config/shutdown moved to the sidebar)")
	}
}

// AGPL-3.0 5(d) requires an interactive interface to carry the legal notice,
// and 13 requires users interacting over a network to be offered the
// Corresponding Source. The footer is in the shared layout, so losing it would
// strip the notice from every page at once. This is a licensing obligation,
// not styling — do not delete it without replacing the notice elsewhere.
func TestLayout_CarriesLicenseAndSourceNotice(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"AGPL-3.0",
		"https://www.gnu.org/licenses/agpl-3.0.html",
		"https://github.com/VrncQuentin/harness",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered layout is missing required license notice %q", want)
		}
	}
}

func TestHandleStatus_LayoutPromptHiddenWhenNoRepoConfigured(t *testing.T) {
	s := NewServer(3000)
	// memRepo is "" by default - the prompt must not render.

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Memory layout incomplete") {
		t.Error("layout prompt must not render when memory repo is unconfigured")
	}
}

func TestHandleStatus_LayoutPromptHiddenWhenLayoutComplete(t *testing.T) {
	s := NewServer(3000)
	root := t.TempDir()
	if err := memory.CreateMissing(root, memory.ExpectedProjectRepoLayout(true)); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	setMemoryRepoPathForTest(s, root)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Memory layout incomplete") {
		t.Errorf("layout prompt must not render when layout is complete:\n%s", body)
	}
}

func TestHandleStatus_LayoutPromptShowsMissingItems(t *testing.T) {
	s := NewServer(3000)
	root := t.TempDir() // entirely empty repo
	setMemoryRepoPathForTest(s, root)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Memory layout incomplete") {
		t.Fatalf("expected layout prompt heading, body:\n%s", body)
	}
	// Each canonical item should appear in the listed missing entries.
	for _, want := range []string{
		"rules.md", "user.md", "facts.md", "agents",
		"sessions.jsonl", "episodes", "index", "index/_episodes", "artifacts",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected missing item %q in rendered body", want)
		}
	}
	if strings.Contains(body, "projects/global") || strings.Contains(body, "global/rules.md") {
		t.Errorf("project memory repo prompt must not render legacy paths, body:\n%s", body)
	}
	// The Create button must POST to /memory/scaffold.
	if !strings.Contains(body, `action="/memory/scaffold"`) {
		t.Error("expected create-missing form pointing at /memory/scaffold")
	}
}

func TestHandleStatus_ShowsScaffoldCreatedToast(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/?scaffold_created=4", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Memory layout updated") {
		t.Error("expected scaffold-success banner on ?scaffold_created>0")
	}
	if !strings.Contains(body, "Created 4 missing items") {
		t.Errorf("expected pluralized count, body:\n%s", body)
	}
}

func TestHandleStatus_ShowsScaffoldErrorBanner(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/?scaffold_err=permission+denied", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Could not scaffold memory layout") {
		t.Error("expected scaffold-error banner on ?scaffold_err=...")
	}
	if !strings.Contains(body, "permission denied") {
		t.Error("expected scaffold error message rendered in banner")
	}
}

func TestHandleMemoryScaffold_RejectsGET(t *testing.T) {
	s := NewServer(3000)
	req := httptest.NewRequest(http.MethodGet, "/memory/scaffold", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryScaffold(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleMemoryScaffold_NoPathConfigured(t *testing.T) {
	s := NewServer(3000)
	// memRepo is "".
	req := httptest.NewRequest(http.MethodPost, "/memory/scaffold", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryScaffold(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/?scaffold_err=") {
		t.Errorf("expected redirect to scaffold_err, got %q", loc)
	}
	if !strings.Contains(loc, "not+configured") {
		t.Errorf("expected error message about missing path, got %q", loc)
	}
}

func TestHandleMemoryScaffold_CreatesMissingItems(t *testing.T) {
	s := NewServer(3000)
	root := t.TempDir()
	setMemoryRepoPathForTest(s, root)

	req := httptest.NewRequest(http.MethodPost, "/memory/scaffold", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryScaffold(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/?scaffold_created=") {
		t.Errorf("expected redirect to scaffold_created, got %q", loc)
	}

	// Re-check: every canonical item now exists on disk.
	for _, item := range []string{
		"rules.md", "user.md", "facts.md", "agents",
		"sessions.jsonl", "episodes", "index", "index/_episodes", "artifacts",
	} {
		abs := filepath.Join(root, filepath.FromSlash(item))
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("expected %s to exist after scaffold: %v", item, err)
		}
	}
}

func TestHandleMemoryScaffold_NoMissingItemsRedirectsCleanly(t *testing.T) {
	s := NewServer(3000)
	root := t.TempDir()
	if err := memory.CreateMissing(root, memory.ExpectedProjectRepoLayout(true)); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	setMemoryRepoPathForTest(s, root)

	req := httptest.NewRequest(http.MethodPost, "/memory/scaffold", nil)
	rec := httptest.NewRecorder()
	s.handleMemoryScaffold(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/" {
		t.Errorf("expected plain redirect to / when nothing to do, got %q", loc)
	}
}

func TestOriginPolicyRejectsCrossOriginMutationsAndEvents(t *testing.T) {
	s := NewServer(0)
	called := false
	h := s.originPolicy(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/config"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/chat/events"},
		{http.MethodGet, "/task/events"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Host = "127.0.0.1:3000"
		req.Header.Set("Origin", "http://attacker.invalid")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	if called {
		t.Fatal("cross-origin request reached handler")
	}
}
