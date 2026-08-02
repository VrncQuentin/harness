package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/queue"
)

// hangClient never sends a token and never closes its stream, so a queue
// worker dispatching through it stays blocked in `range tokenCh` regardless of
// context cancellation. started closes on the first Complete call so tests can
// wait until the worker is genuinely stuck. Releasing closes the stream, which
// lets the worker finish and exit.
type hangClient struct {
	ch      chan inference.Token
	started chan struct{}
	once    sync.Once
}

func (c *hangClient) Complete(context.Context, inference.CompletionRequest) (<-chan inference.Token, error) {
	c.once.Do(func() { close(c.started) })
	return c.ch, nil
}

func (c *hangClient) release() { close(c.ch) }

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
