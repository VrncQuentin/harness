package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingMemoryStore wraps a stubMemoryStore and blocks on the first Read so
// a test can pause a request mid-operation and republish dependencies.
type blockingMemoryStore struct {
	*stubMemoryStore
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMemoryStore) Read(p string) ([]byte, error) {
	close(b.entered)
	<-b.release
	return b.stubMemoryStore.Read(p)
}

// signalRecorder is an httptest.ResponseRecorder that closes done on the first
// WriteHeader, so a test can wait barrier-style for a request to finish.
type signalRecorder struct {
	*httptest.ResponseRecorder
	done chan struct{}
	once sync.Once
}

func (r *signalRecorder) WriteHeader(code int) {
	r.once.Do(func() { close(r.done) })
	r.ResponseRecorder.WriteHeader(code)
}

// TestSnapshot_RequestUsesConsistentDependencies proves that one request uses
// one captured snapshot even when the runtime publishes a new generation
// between two logical operations. A publication lands between the promotion's
// memory-store read and its git commit; the commit must still go to the same
// generation's committer, never to a later publication's.
func TestSnapshot_RequestUsesConsistentDependencies(t *testing.T) {
	s := NewServer(3000)

	storeA := &blockingMemoryStore{
		stubMemoryStore: newStubMemoryStore(map[string]string{"facts.md": "fact A\n"}),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	committerA := &stubCommitter{}
	storeB := newStubMemoryStore(map[string]string{"facts.md": "fact B\n"})
	committerB := &stubCommitter{}

	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		MemoryStore: storeA,
		Committer:   committerA,
	}})

	form := url.Values{"text": {"new project fact"}}
	rec := &signalRecorder{ResponseRecorder: httptest.NewRecorder(), done: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/memory/promote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	go s.handlePromoteFact(rec, req)

	select {
	case <-storeA.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the memory-store read")
	}

	// Publish generation B while the request is blocked between the store
	// operation and the commit operation.
	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		MemoryStore: storeB,
		Committer:   committerB,
	}})
	close(storeA.release)

	select {
	case <-rec.done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete after the barrier was lifted")
	}

	storeA.mu.Lock()
	aWritten, aHas := storeA.files["facts.md"]
	storeA.mu.Unlock()
	storeB.mu.Lock()
	bWritten, bHas := storeB.files["facts.md"]
	bLastWrite := storeB.lastWritePath
	storeB.mu.Unlock()

	if !aHas || !strings.Contains(aWritten, "new project fact") {
		t.Fatalf("generation A store was not updated by the request: has=%v data=%q", aHas, aWritten)
	}
	if len(committerA.messages) != 1 {
		t.Fatalf("generation A committer was not called exactly once: %#v", committerA.messages)
	}
	if bHas && strings.Contains(bWritten, "new project fact") {
		t.Fatalf("generation B store was updated by a request that captured generation A: data=%q", bWritten)
	}
	if bLastWrite != "" {
		t.Fatalf("generation B store received a write: path=%q", bLastWrite)
	}
	if len(committerB.messages) != 0 {
		t.Fatalf("generation B committer was called: %#v", committerB.messages)
	}
}

// countingSnapshotProvider serves one snapshot and counts how often the lease
// is acquired and released, closing firstRelease on the first release so a
// test can wait barrier-style for a detached goroutine to drop its lease.
type countingSnapshotProvider struct {
	snap ServiceDeps

	mu           sync.Mutex
	acquires     int
	releases     int
	firstRelease chan struct{}
	releaseOnce  sync.Once
}

func (p *countingSnapshotProvider) AcquireUISnapshot() (ServiceDeps, func()) {
	p.mu.Lock()
	p.acquires++
	p.mu.Unlock()
	return p.snap, func() {
		p.mu.Lock()
		p.releases++
		p.mu.Unlock()
		p.releaseOnce.Do(func() { close(p.firstRelease) })
	}
}

func (p *countingSnapshotProvider) acquireCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acquires
}

func (p *countingSnapshotProvider) releaseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.releases
}

// TestSnapshot_AgentsPageUsesAcquisitionActiveAgent proves the /agents page
// marks the agent from the acquisition-scoped snapshot rather than re-reading
// the registry's live selection. The registry's Active() returns "reviewer"
// (as if a concurrent /agents/active switched after this snapshot was
// captured), while the snapshot's ActiveAgent is "coder"; the page must mark
// coder active.
func TestSnapshot_AgentsPageUsesAcquisitionActiveAgent(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("reviewer",
		AgentInfo{Name: "coder", Origin: "global"},
		AgentInfo{Name: "reviewer", Origin: "global"},
	)
	setSnapshotForTest(s, ServiceDeps{
		AgentRegistry: reg,
		ActiveAgent:   "coder",
	})

	rec := httptest.NewRecorder()
	s.handleAgents(rec, httptest.NewRequest(http.MethodGet, "/agents", nil))

	body := rec.Body.String()
	idxCoder := strings.Index(body, `<h3 class="agent-name">coder</h3>`)
	idxReviewer := strings.Index(body, `<h3 class="agent-name">reviewer</h3>`)
	if idxCoder < 0 || idxReviewer < 0 {
		t.Fatalf("agents page missing cards: coder=%d reviewer=%d", idxCoder, idxReviewer)
	}
	coderCard := body[idxCoder:idxReviewer]
	if !strings.Contains(coderCard, "badge badge-ok") {
		t.Fatal("coder card is not marked active; the page used the registry's live active agent instead of the snapshot's")
	}
	if strings.Contains(body[idxReviewer:], "badge badge-ok") {
		t.Fatal("reviewer card is marked active, but the snapshot captured coder as active")
	}
}

// TestSnapshot_ChatPageUsesAcquisitionActiveAgent proves the /chat page
// renders the acquisition-scoped active agent. Same setup as the agents test:
// the registry reports reviewer live while the snapshot captured coder.
func TestSnapshot_ChatPageUsesAcquisitionActiveAgent(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("reviewer",
		AgentInfo{Name: "coder"},
		AgentInfo{Name: "reviewer"},
	)
	setSnapshotForTest(s, ServiceDeps{
		AgentRegistry: reg,
		ChatRunner:    &stubChatRunner{},
		ActiveAgent:   "coder",
	})

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `data-agent="coder"`) {
		t.Fatalf("chat page did not render the snapshot's active agent; expected data-agent=coder, body:\n%s", body)
	}
	if strings.Contains(body, `data-agent="reviewer"`) {
		t.Fatal("chat page rendered the registry's live active agent instead of the acquisition-scoped one")
	}
}

// blockedChatRunner signals its first invocation through started and blocks
// until release is closed, so a test can hold the detached goroutine open and
// observe the lease state while the stream is running.
type blockedChatRunner struct {
	started chan struct{}
	release chan struct{}

	mu      sync.Mutex
	gotMsgs []ChatMessage
}

func (r *blockedChatRunner) Run(_ context.Context, _ string, sessionID string, conv []ChatMessage) (string, <-chan ChatToken, error) {
	r.mu.Lock()
	r.gotMsgs = append([]ChatMessage(nil), conv...)
	r.mu.Unlock()
	close(r.started)
	<-r.release
	ch := make(chan ChatToken, 1)
	ch <- ChatToken{Done: true}
	close(ch)
	return sessionID, ch, nil
}

// blockedTaskRunner mirrors blockedChatRunner for /task/send.
type blockedTaskRunner struct {
	started chan struct{}
	release chan struct{}

	mu      sync.Mutex
	gotMsgs []ChatMessage
}

func (r *blockedTaskRunner) RunTask(_ context.Context, _ string, sessionID string, conv []ChatMessage) (string, <-chan TaskEvent, error) {
	r.mu.Lock()
	r.gotMsgs = append([]ChatMessage(nil), conv...)
	r.mu.Unlock()
	close(r.started)
	<-r.release
	ch := make(chan TaskEvent, 1)
	ch <- TaskEvent{Type: TaskEventDone}
	close(ch)
	return sessionID, ch, nil
}

func (r *blockedTaskRunner) CancelTask(string) error                    { return nil }
func (r *blockedTaskRunner) ApplyApproval(string, string, string) error { return nil }

// TestSnapshot_DetachedGoroutineCapturesSnapshotBeforeStart proves that
// /chat/send and /task/send capture their snapshot before launch, transfer the
// lease to the detached goroutine, keep the lease held for the whole stream,
// and release it exactly once when the stream ends. The counting provider and
// blocked runner make an early release, a leaked release, or a double release
// observable.
func TestSnapshot_DetachedGoroutineCapturesSnapshotBeforeStart(t *testing.T) {
	t.Run("chat", func(t *testing.T) { testDetachedGoroutineLease(t, "chat") })
	t.Run("task", func(t *testing.T) { testDetachedGoroutineLease(t, "task") })
}

func testDetachedGoroutineLease(t *testing.T, kind string) {
	t.Helper()
	s := NewServer(3000)

	storeA := &stubSessionStore{liveResult: []ChatMessage{{Role: "user", Content: "from-A"}}}
	chatRunner := &blockedChatRunner{started: make(chan struct{}), release: make(chan struct{})}
	taskRunner := &blockedTaskRunner{started: make(chan struct{}), release: make(chan struct{})}

	snap := ServiceDeps{SessionStore: storeA}
	if kind == "chat" {
		snap.ChatRunner = chatRunner
	} else {
		snap.TaskRunner = taskRunner
	}
	provider := &countingSnapshotProvider{snap: snap, firstRelease: make(chan struct{})}
	s.SetSnapshotProvider(provider)

	form := url.Values{
		"message":    {"hello"},
		"agent":      {"coder"},
		"session_id": {"s1"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/"+kind+"/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if kind == "chat" {
		s.handleChatSend(rec, req)
	} else {
		s.handleTaskSend(rec, req)
	}

	// The HTTP handler has returned; the detached goroutine owns the lease.
	var started chan struct{}
	var releaseCh chan struct{}
	if kind == "chat" {
		started, releaseCh = chatRunner.started, chatRunner.release
	} else {
		started, releaseCh = taskRunner.started, taskRunner.release
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("detached goroutine never invoked the captured runner")
	}

	// Zero releases while the stream is still running.
	if got := provider.releaseCount(); got != 0 {
		t.Fatalf("lease released while the stream was running: releases=%d", got)
	}

	// Let the stream finish; the lease must be released exactly once.
	close(releaseCh)
	select {
	case <-provider.firstRelease:
	case <-time.After(2 * time.Second):
		t.Fatal("detached goroutine never released the lease")
	}
	if got := provider.releaseCount(); got != 1 {
		t.Fatalf("lease releases = %d, want exactly 1", got)
	}
	if got := provider.acquireCount(); got != 1 {
		t.Fatalf("snapshot acquires = %d, want 1", got)
	}

	// The conversation must come from generation A's session store, captured
	// before the handler returned.
	var gotMsgs []ChatMessage
	if kind == "chat" {
		chatRunner.mu.Lock()
		gotMsgs = append([]ChatMessage(nil), chatRunner.gotMsgs...)
		chatRunner.mu.Unlock()
	} else {
		taskRunner.mu.Lock()
		gotMsgs = append([]ChatMessage(nil), taskRunner.gotMsgs...)
		taskRunner.mu.Unlock()
	}
	want := []ChatMessage{
		{Role: "user", Content: "from-A"},
		{Role: "user", Content: "hello"},
	}
	if !reflect.DeepEqual(gotMsgs, want) {
		t.Fatalf("detached goroutine got conversation %+v, want %+v (both turns from generation A)", gotMsgs, want)
	}
}

// TestSnapshot_DetachedGoroutineReleaseOnPreLaunchError proves that every
// /chat/send and /task/send error path before the goroutine is launched
// releases the acquired lease exactly once, so no generation is leaked.
func TestSnapshot_DetachedGoroutineReleaseOnPreLaunchError(t *testing.T) {
	for _, kind := range []string{"chat", "task"} {
		for _, tc := range []struct {
			name       string
			form       url.Values
			withRunner bool
			wantStatus int
		}{
			{name: "empty message", form: url.Values{"message": {""}}, withRunner: true, wantStatus: http.StatusBadRequest},
			{name: "no runner", form: url.Values{"message": {"hi"}}, withRunner: false, wantStatus: http.StatusServiceUnavailable},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s := NewServer(3000)
				snap := ServiceDeps{}
				if tc.withRunner {
					if kind == "chat" {
						snap.ChatRunner = &stubChatRunner{}
					} else {
						snap.TaskRunner = &recordingTaskRunner{}
					}
				}
				provider := &countingSnapshotProvider{snap: snap, firstRelease: make(chan struct{})}
				s.SetSnapshotProvider(provider)

				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/"+kind+"/send", strings.NewReader(tc.form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if kind == "chat" {
					s.handleChatSend(rec, req)
				} else {
					s.handleTaskSend(rec, req)
				}

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if got := provider.acquireCount(); got != 1 {
					t.Fatalf("snapshot acquires = %d, want 1", got)
				}
				select {
				case <-provider.firstRelease:
				case <-time.After(2 * time.Second):
					t.Fatal("pre-launch error path never released the lease")
				}
				if got := provider.releaseCount(); got != 1 {
					t.Fatalf("lease releases = %d, want exactly 1 on a pre-launch error", got)
				}
			})
		}
	}
}
