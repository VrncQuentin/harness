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

// snapshotChatRunner records the conversation it receives and signals its first
// invocation through started.
type snapshotChatRunner struct {
	mu        sync.Mutex
	started   chan struct{}
	sessionID string
	msgs      []ChatMessage
}

func (r *snapshotChatRunner) Run(_ context.Context, _ string, sessionID string, conv []ChatMessage) (string, <-chan ChatToken, error) {
	r.mu.Lock()
	r.sessionID = sessionID
	r.msgs = append([]ChatMessage(nil), conv...)
	r.mu.Unlock()
	close(r.started)
	ch := make(chan ChatToken, 1)
	ch <- ChatToken{Done: true}
	close(ch)
	return sessionID, ch, nil
}

// TestSnapshot_DetachedGoroutineCapturesSnapshotBeforeStart proves that
// /chat/send captures its snapshot before launch, keeps using it after the
// HTTP handler returns, and passes the generation-A conversation to the
// detached goroutine even though generation B is published immediately after
// the handler responds.
func TestSnapshot_DetachedGoroutineCapturesSnapshotBeforeStart(t *testing.T) {
	s := NewServer(3000)

	runnerA := &snapshotChatRunner{started: make(chan struct{})}
	runnerB := &snapshotChatRunner{started: make(chan struct{})}
	storeA := &stubSessionStore{liveResult: []ChatMessage{{Role: "user", Content: "from-A"}}}
	storeB := &stubSessionStore{liveResult: []ChatMessage{{Role: "user", Content: "from-B"}}}

	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		ChatRunner:   runnerA,
		SessionStore: storeA,
	}})

	form := url.Values{
		"message":    {"hello"},
		"agent":      {"coder"},
		"session_id": {"s1"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)

	// The HTTP handler has returned. Publish generation B before the detached
	// goroutine necessarily starts.
	s.SetSnapshotProvider(&staticSnapshotProvider{snap: ServiceDeps{
		ChatRunner:   runnerB,
		SessionStore: storeB,
	}})

	select {
	case <-runnerA.started:
	case <-time.After(2 * time.Second):
		t.Fatal("detached goroutine never invoked the captured runner")
	}

	runnerA.mu.Lock()
	gotMsgs := append([]ChatMessage(nil), runnerA.msgs...)
	runnerA.mu.Unlock()

	want := []ChatMessage{
		{Role: "user", Content: "from-A"},
		{Role: "user", Content: "hello"},
	}
	if !reflect.DeepEqual(gotMsgs, want) {
		t.Fatalf("detached goroutine got conversation %+v, want %+v (both turns from generation A)", gotMsgs, want)
	}

	select {
	case <-runnerB.started:
		t.Fatal("runner B was invoked despite the snapshot being captured before generation B was published")
	default:
	}
}
