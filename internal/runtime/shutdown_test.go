package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/session"
)

// hangClient never sends a token and never closes its stream, so a queue
// worker dispatching through it stays blocked in the token loop regardless of
// context cancellation. started closes on the first Complete call so tests can
// wait until the worker is genuinely stuck. Releasing closes the stream, which
// lets the worker finish and exit.
type hangClient struct {
	ch          chan inference.Token
	started     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (c *hangClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	c.startOnce.Do(func() { close(c.started) })
	return c.ch, nil
}

func (c *hangClient) release() {
	c.releaseOnce.Do(func() { close(c.ch) })
}

// newHangQueue builds a started queue whose worker will block on a hung
// client once a request is dispatched. The caller must release the client so
// the worker can exit before the test ends.
func newHangQueue(t *testing.T) (*queue.Queue, *hangClient) {
	t.Helper()
	client := &hangClient{ch: make(chan inference.Token), started: make(chan struct{})}
	q := queue.New(4, client)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("hang queue Start: %v", err)
	}
	return q, client
}

// TestShutdown_StopsNewAdmissionsFirst verifies that shutdown transitions to a
// non-admitting state before anything is drained: the moment admissions close,
// a new enqueue through the production request queue is refused.
func TestShutdown_StopsNewAdmissionsFirst(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	q, client := newHangQueue(t)
	defer client.release()
	rt.reqQueue = q

	rootCtx, rootCancel := context.WithCancel(context.Background())
	_ = rootCtx
	defer rootCancel()

	atAdmissions := make(chan struct{})
	resume := make(chan struct{})
	rt.shutdownHook = func(step string) {
		if step == "admissions-closed" {
			close(atAdmissions)
			<-resume
		}
	}

	done := make(chan ShutdownResult, 1)
	go func() { done <- rt.Shutdown(rootCancel, time.Second) }()

	<-atAdmissions
	err := q.Enqueue(queue.Request{Response: make(chan inference.Token, 1), Ctx: context.Background()})
	if !errors.Is(err, queue.ErrStopped) {
		t.Fatalf("Enqueue after shutdown began = %v, want ErrStopped (new work must be refused once shutdown begins)", err)
	}
	close(resume)
	result := <-done
	rt.shutdownHook = nil
	if !result.Completed {
		t.Fatalf("shutdown after the admission barrier = %+v, want completed", result)
	}
}

// TestShutdown_CancelsBeforeWaiting verifies the shutdown lifecycle order:
// admissions close, then the root context is cancelled, then the bounded
// drains run, then resources are released. Cancelling the root context before
// waiting for tasks, sessions, queue work, API handlers, and process managers
// is what lets those waits terminate promptly.
func TestShutdown_CancelsBeforeWaiting(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	rootCtx, rootCancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var seq []string
	rt.shutdownHook = func(step string) {
		mu.Lock()
		seq = append(seq, step)
		mu.Unlock()
	}

	result := rt.Shutdown(rootCancel, time.Second)
	rt.shutdownHook = nil
	if !result.Completed {
		t.Fatalf("shutdown = %+v, want completed", result)
	}
	if rootCtx.Err() == nil {
		t.Fatal("Shutdown did not cancel the root context")
	}

	mu.Lock()
	got := append([]string(nil), seq...)
	mu.Unlock()
	want := []string{
		"admissions-closed",
		"root-cancelled",
		"tasks-cancelled",
		"sessions-flushed",
		"api-stopped",
		"queue-wait",
		"generation-released",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown lifecycle = %v, want %v (the root context must be cancelled before any drain/wait)", got, want)
	}
}

// TestShutdown_BoundedWait verifies that every shutdown wait is context-aware
// or has an explicit bound: with a queue worker permanently stuck on a hung
// client and API servers that never confirm termination, Shutdown still
// returns within the drain budget instead of hanging.
func TestShutdown_BoundedWait(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	q, client := newHangQueue(t)
	defer client.release()
	rt.reqQueue = q
	if err := q.Enqueue(queue.Request{Response: make(chan inference.Token, 4), Ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	<-client.started

	// API servers whose Stop never confirms termination must not extend the
	// shutdown beyond the bound either.
	srv := newServedAPIServer(t, freeTCPPort(t))
	rt.mu.Lock()
	rt.apiServer = srv
	rt.mu.Unlock()
	rt.stopAPIServer = func(*api.Server) bool { return false }

	start := time.Now()
	result := rt.Shutdown(nil, 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v with hung components; every wait must be bounded", elapsed)
	}
	if !result.TimedOut {
		t.Fatal("shutdown with hung components must report a timed-out drain")
	}
	if result.Completed {
		t.Fatal("shutdown with hung components must not claim completion")
	}

	// Release the hung components and retry so the retained generation is
	// released and the temp repo can be removed during cleanup.
	rt.stopAPIServer = func(s *api.Server) bool { return s.Stop() }
	client.release()
	if retry := rt.Shutdown(nil, 2*time.Second); !retry.Completed {
		t.Fatalf("retry after clearing the hangs = %+v, want completed", retry)
	}
}

// TestShutdown_DrainTimeoutDoesNotCloseInUse verifies that a timed-out drain
// retains ownership of the generation its in-flight work still uses. A held
// snapshot lease pins the generation; after the drain times out the runtime
// must still own it and the retained readers must perform real operations.
// Only after the held lease is released and a later Shutdown retry succeeds is
// the generation closed.
func TestShutdown_DrainTimeoutDoesNotCloseInUse(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	root := projectsPathForApplied(t, rt)
	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("retained"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The held snapshot lease is the in-flight work that pins the generation.
	snap, release := rt.AcquireUISnapshot()

	q, client := newHangQueue(t)
	rt.reqQueue = q
	if err := q.Enqueue(queue.Request{Response: make(chan inference.Token, 4), Ctx: context.Background()}); err != nil {
		release()
		t.Fatal(err)
	}
	<-client.started

	result := rt.Shutdown(nil, 200*time.Millisecond)
	if !result.TimedOut {
		release()
		t.Fatal("expected the drain to time out")
	}

	rt.mu.Lock()
	retainedGen := rt.gen
	rt.mu.Unlock()
	if retainedGen == nil {
		release()
		t.Fatal("timed-out shutdown dropped generation ownership")
	}

	// Real operations through the retained old generation before releasing
	// the held lease.
	if b, err := snap.MemoryStore.Read("known.txt"); err != nil || string(b) != "retained" {
		release()
		t.Fatalf("held snapshot read = %q, %v", b, err)
	}
	if _, err := rt.activeMem.Read("rules.md"); err != nil {
		release()
		t.Fatalf("retained generation reader failed after timed-out shutdown: %v", err)
	}

	// Unblock the hung worker and retry: ownership is released only once the
	// retry confirms everything is quiescent.
	client.release()
	retry := rt.Shutdown(nil, time.Second)
	release()
	if !retry.Completed {
		t.Fatalf("retry after the hang cleared = %+v, want completed", retry)
	}
	rt.mu.Lock()
	releasedGen := rt.gen
	rt.mu.Unlock()
	if releasedGen != nil {
		t.Fatal("generation not released after a completed retry")
	}
}

// TestShutdown_NoUnboundedStopAfterDrainFailure verifies that a failed bounded
// drain is never followed by an unbounded queue stop that defeats the timeout.
// With the queue worker stuck, Shutdown must return within the drain budget,
// retain the queue for a later retry, and only release it once the worker
// clears.
func TestShutdown_NoUnboundedStopAfterDrainFailure(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	q, client := newHangQueue(t)
	rt.reqQueue = q
	if err := q.Enqueue(queue.Request{Response: make(chan inference.Token, 4), Ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	<-client.started

	start := time.Now()
	result := rt.Shutdown(nil, 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v after the bounded drain failed; an unbounded queue stop defeated the timeout", elapsed)
	}
	if !result.TimedOut {
		t.Fatal("shutdown with a hung queue worker must report a timed-out drain")
	}

	// The queue is retained for a later retry, not dropped on the failure.
	rt.mu.Lock()
	retained := rt.reqQueue
	rt.mu.Unlock()
	if retained != q {
		t.Fatal("queue ownership was dropped after a failed bounded drain")
	}

	// Once the worker clears, a retry completes and releases the queue.
	client.release()
	retry := rt.Shutdown(nil, time.Second)
	if !retry.Completed {
		t.Fatalf("retry after the worker cleared = %+v, want completed", retry)
	}
	rt.mu.Lock()
	released := rt.reqQueue
	rt.mu.Unlock()
	if released != nil {
		t.Fatal("queue not released after a completed retry")
	}
}

// TestShutdown_APIOwnershipPreservedToTermination verifies that API ownership
// is preserved until termination is confirmed for every class of server: the
// active server, a pending-retired server, and a previously timed-out server.
// A shutdown whose Stop calls never confirm termination retains all three; the
// retained active server still serves. A later shutdown that confirms
// termination releases every slot.
func TestShutdown_APIOwnershipPreservedToTermination(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	activePort := freeTCPPort(t)
	pendingPort := freeTCPPort(t)
	retiredPort := freeTCPPort(t)
	active := newServedAPIServer(t, activePort)
	pending := newServedAPIServer(t, pendingPort)
	retired := newServedAPIServer(t, retiredPort)

	rt.mu.Lock()
	rt.apiServer = active
	rt.pendingRetiredAPI = []*api.Server{pending}
	rt.retiredAPI = []*api.Server{retired}
	rt.mu.Unlock()
	rt.stopAPIServer = func(*api.Server) bool { return false }

	result := rt.Shutdown(nil, 100*time.Millisecond)
	if !result.TimedOut {
		t.Fatal("shutdown with unconfirmed API servers must report a timed-out drain")
	}

	// All three classes retain ownership.
	rt.mu.Lock()
	gotActive := rt.apiServer
	gotRetired := append([]*api.Server(nil), rt.retiredAPI...)
	rt.mu.Unlock()
	if gotActive != active {
		t.Fatal("active API server ownership was released before termination was confirmed")
	}
	if len(gotRetired) != 2 {
		t.Fatalf("retired API servers = %d, want 2 (pending-retired + previously timed-out must retain ownership)", len(gotRetired))
	}

	// The retained active server still serves real requests.
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", activePort))
	if err != nil {
		t.Fatalf("retained active API server no longer serves: %v", err)
	}
	_ = resp.Body.Close()

	// A later shutdown that confirms termination releases every slot.
	rt.stopAPIServer = func(s *api.Server) bool { return s.Stop() }
	retry := rt.Shutdown(nil, time.Second)
	if !retry.Completed {
		t.Fatalf("retry with confirming stops = %+v, want completed", retry)
	}
	rt.mu.Lock()
	gotActive = rt.apiServer
	gotRetired = append([]*api.Server(nil), rt.retiredAPI...)
	rt.mu.Unlock()
	if gotActive != nil {
		t.Fatal("active API server ownership not released after confirmed termination")
	}
	if len(gotRetired) != 0 {
		t.Fatalf("retired API servers = %d after confirmed termination, want 0", len(gotRetired))
	}
}

// hangOnceThenDoneClient hangs on the first Complete call (returning an open
// stream that never sends or closes) and completes on later calls, so a save
// that holds saveMu can be released by cancelling its context and a later
// flush can succeed.
type hangOnceThenDoneClient struct {
	mu      sync.Mutex
	calls   int
	hangCh  chan inference.Token
	started chan struct{}
	once    sync.Once
}

func (c *hangOnceThenDoneClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 1 {
		c.once.Do(func() { close(c.started) })
		return c.hangCh, nil
	}
	ch := make(chan inference.Token, 2)
	ch <- inference.Token{Content: "summary"}
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

// errThenDoneClient fails the first Complete call and completes later ones,
// so a session save fails on the first flush attempt and succeeds on a retry.
type errThenDoneClient struct {
	mu    sync.Mutex
	calls int
}

func (c *errThenDoneClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 1 {
		return nil, errors.New("summarizer backend down")
	}
	ch := make(chan inference.Token, 2)
	ch <- inference.Token{Content: "summary"}
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

// blockThenDoneClient blocks the first Complete call until released, then
// completes, so a single session save stays in flight across shutdown retries
// and can be released to count exactly how many saves ran.
type blockThenDoneClient struct {
	mu          sync.Mutex
	calls       int
	block       chan struct{}
	started     chan struct{}
	startedOnce sync.Once
	released    sync.Once
}

func (c *blockThenDoneClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 1 {
		c.startedOnce.Do(func() { close(c.started) })
		<-c.block
	}
	ch := make(chan inference.Token, 2)
	ch <- inference.Token{Content: "summary"}
	ch <- inference.Token{Done: true}
	close(ch)
	return ch, nil
}

func (c *blockThenDoneClient) release() {
	c.released.Do(func() { close(c.block) })
}

func (c *blockThenDoneClient) callsCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newGenerationedManagerForTest builds a session manager over a fresh project
// repo reader wired to client, and publishes a generation that owns the reader,
// so shutdown release semantics can be exercised directly.
func newGenerationedManagerForTest(t *testing.T, cfg *config.Config, client inference.Client) (*Runtime, *session.Manager, string) {
	t.Helper()
	root := initRuntimeProjectRepo(t)
	rt := New(*cfg, &runtimeConfigStore{cfg: cfg, saved: true}, LogRings{})
	rt.projectStore = &runtimeProjectStoreStub{projects: map[string]project.Project{
		project.GlobalSlug: {Slug: project.GlobalSlug, DisplayName: "Global", MemoryRepoPath: root},
	}}
	gitRepo, mgr, _ := newSessionManagerForTest(t, rt, root, client)
	t.Cleanup(func() { _ = gitRepo.Close() })
	rt.setSessionManager(mgr)
	rt.gen = &generation{readers: []memory.Repo{rt.activeMem}}
	rt.gen.acquire()
	return rt, mgr, root
}

// TestShutdown_SessionFlushBounded verifies that the session flush is
// genuinely bounded: with a save in flight holding saveMu (its summarizer
// blocked on an unresponsive inference client), Shutdown returns within the
// drain budget and retains the session manager and generation for a retry.
func TestShutdown_SessionFlushBounded(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	client := &hangOnceThenDoneClient{hangCh: make(chan inference.Token), started: make(chan struct{})}
	rt, mgr, _ := newGenerationedManagerForTest(t, &cfg, client)
	t.Cleanup(func() { rt.Stop() })

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	// A save in flight: it acquires saveMu and its summarizer blocks on the
	// unresponsive client, so no later flush can acquire the lock.
	saveCtx, saveCancel := context.WithCancel(context.Background())
	defer saveCancel()
	saveDone := make(chan error, 1)
	go func() {
		_, err := mgr.Save(saveCtx, s.ID)
		saveDone <- err
	}()
	<-client.started

	start := time.Now()
	result := rt.Shutdown(nil, 100*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("Shutdown took %v while a session save held saveMu; the flush must be bounded", elapsed)
	}
	if !result.TimedOut {
		t.Fatal("shutdown with an in-flight session save must report a timed-out drain")
	}

	// Ownership is retained: the session manager and generation stay for a
	// later Shutdown retry.
	if rt.SessionManager() != mgr {
		t.Fatal("session manager ownership dropped while its save was still in flight")
	}
	rt.mu.Lock()
	retainedGen := rt.gen
	rt.mu.Unlock()
	if retainedGen == nil {
		t.Fatal("generation dropped while a dependent session manager was retained")
	}

	// Releasing the in-flight save lets a retry flush complete and release.
	saveCancel()
	if err := <-saveDone; err == nil {
		t.Fatal("cancelled save should report an error")
	}
	if retry := rt.Shutdown(nil, 2*time.Second); !retry.Completed {
		t.Fatalf("retry after releasing the in-flight save = %+v, want completed", retry)
	}
}

// TestShutdown_RetryAfterFlushFailureUsesRetainedReader verifies that a failed
// first session flush retains the generation (its readers stay open) and that
// a later Shutdown retry succeeds by saving through the still-open retained
// reader.
func TestShutdown_RetryAfterFlushFailureUsesRetainedReader(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	client := &errThenDoneClient{}
	rt, mgr, root := newGenerationedManagerForTest(t, &cfg, client)
	t.Cleanup(func() { rt.Stop() })

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "known.txt"), []byte("retained"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := rt.Shutdown(nil, 100*time.Millisecond)
	if !first.TimedOut {
		t.Fatal("a failed first session flush must report a timed-out drain")
	}

	// The retained reader is still open and performs a real operation.
	if b, err := rt.activeMem.Read("known.txt"); err != nil || string(b) != "retained" {
		t.Fatalf("retained reader read = %q, %v", b, err)
	}

	// The retry saves the session through the still-open retained reader.
	second := rt.Shutdown(nil, 2*time.Second)
	if !second.Completed {
		t.Fatalf("retry after the flush failure = %+v, want completed", second)
	}
	rt.mu.Lock()
	releasedGen := rt.gen
	rt.mu.Unlock()
	if releasedGen != nil {
		t.Fatal("generation not released after a completed retry")
	}
}

// TestShutdown_SingleFlushAcrossRetries verifies that the runtime owns exactly
// one in-flight session flush across repeated shutdown attempts: while the
// first flush remains blocked, each retry joins it instead of stacking another
// (so no extra saves run), and releasing the block produces exactly one
// successful durable save.
func TestShutdown_SingleFlushAcrossRetries(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	client := &blockThenDoneClient{block: make(chan struct{}), started: make(chan struct{})}
	rt, mgr, root := newGenerationedManagerForTest(t, &cfg, client)
	t.Cleanup(func() { rt.Stop() })

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	// First attempt starts the single flush, whose save blocks on the client.
	if result := rt.Shutdown(nil, 100*time.Millisecond); !result.TimedOut {
		t.Fatal("first shutdown with a blocked flush must time out")
	}
	<-client.started

	// Repeated retries while the flush is still blocked must join it, not
	// start additional flushes: the save is invoked exactly once so far.
	for i := 0; i < 3; i++ {
		if result := rt.Shutdown(nil, 50*time.Millisecond); !result.TimedOut {
			t.Fatalf("retry %d with a blocked flush must time out", i+1)
		}
	}
	if got := client.callsCount(); got != 1 {
		t.Fatalf("save invocations across retries = %d, want 1 (retries must join the single in-flight flush)", got)
	}

	// Release the block: the single flush completes with exactly one save.
	client.release()
	retry := rt.Shutdown(nil, 3*time.Second)
	if !retry.Completed {
		t.Fatalf("shutdown after the flush cleared = %+v, want completed", retry)
	}
	if got := client.callsCount(); got != 1 {
		t.Fatalf("save invocations after clearing = %d, want exactly 1", got)
	}

	// Exactly one durable session log record exists.
	logPath := filepath.Join(root, "sessions.jsonl")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1
	if strings.TrimSpace(string(b)) == "" {
		lines = 0
	}
	if lines != 1 {
		t.Fatalf("session log records = %d, want exactly 1 (a second flush would have duplicated the save)", lines)
	}
}

// newServedAPIServer builds a real API server on a real loopback port and
// serves it, so tests can make real HTTP requests against a retained server.
func newServedAPIServer(t *testing.T, port int) *api.Server {
	t.Helper()
	srv := api.NewServer(port, nil, nil, nil)
	if err := srv.Bind(context.Background()); err != nil {
		t.Fatalf("api Bind: %v", err)
	}
	srv.Serve()
	t.Cleanup(func() { srv.Stop() })
	return srv
}
