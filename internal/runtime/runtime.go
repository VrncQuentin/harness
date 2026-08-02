// Package runtime owns the mutable service graph behind the harness.
package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/logbuf"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/prompt"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/session"
	"github.com/VrncQuentin/harness/internal/ui"
)

// generation owns the concrete resources of one reload cycle: readers,
// assembler, session manager, and the immutable UI dependency snapshot. The
// publisher holds one lease for as long as the generation is installed;
// operations acquire an additional lease before using any generation resource
// and release it after. When the lease count reaches zero, the generation's
// readers and owned handles are closed.
type generation struct {
	readers    []memory.Repo
	assembler  *prompt.DiskAssembler
	sessionMgr *session.Manager
	handles    []io.Closer
	// uiSnap is the complete set of generation-bound UI dependencies. It is
	// captured once when the generation is published and handed out (with a
	// lease) by AcquireUISnapshot, so every UI request observes one coherent
	// generation.
	uiSnap ui.ServiceDeps
	leases atomic.Int64
}

func (g *generation) acquire() { g.leases.Add(1) }

func (g *generation) release() {
	if g.leases.Add(-1) == 0 {
		closeReaders(g.readers...)
		for _, h := range g.handles {
			_ = h.Close()
		}
	}
}

// ErrConfigStoreUnavailable is surfaced when the harness DB could not be
// opened, so the user sees one consistent message in the status page and the
// config editor.
var ErrConfigStoreUnavailable = errors.New("config store unavailable (harness.db could not be opened)")

// EventBufferSize absorbs process-manager startup bursts without forcing
// managers to block while the UI event forwarder catches up.
const EventBufferSize = 64

// LogRings groups the in-memory log buffers shared by services and the UI.
type LogRings struct {
	Log   *logbuf.Ring
	Llama *logbuf.Ring
	Embed *logbuf.Ring
}

// Runtime holds mutable service references that the retry/save callback
// reconfigures in place. A mutex guards all fields because callbacks run on
// HTTP goroutines while event forwarding and metrics read managers and queue.
type Runtime struct {
	mu           sync.Mutex
	cfg          config.Config
	cfgStore     config.Store
	logRings     LogRings
	projectStore project.Store
	inferClient  inference.Client

	llamaMgr *proc.Manager
	embedMgr *proc.Manager
	reqQueue *queue.Queue
	started  bool

	globalMem  memory.Repo
	activeMem  memory.Repo
	agentReg   *agent.DiskRegistry
	assembler  *prompt.DiskAssembler
	apiServer  *api.Server
	sessionMu  sync.RWMutex
	sessionMg  *session.Manager
	taskRunner *taskRunnerAdapter
	gen        *generation

	// applyMu serializes ApplyConfig end-to-end: validation, preparation,
	// quiesce, commit, and retirement are one transaction, so two concurrent
	// applies cannot interleave their decisions or their process changes.
	applyMu sync.Mutex

	// applied records the facts about the live system the last successful
	// apply committed: committed config, active project, preferred effective
	// model, and the actually-running llama/embedder process configuration. It
	// is the source of truth for what "old" means on the next apply; the
	// runtime never reconstructs the old state from the mutable stores.
	applied *appliedState

	// pendingRetiredAPI holds API servers retired by the current commit; their
	// Stop runs right after the commit, outside rt.mu.
	pendingRetiredAPI []*api.Server

	// retiredAPI holds API servers whose shutdown did not confirm termination
	// within the timeout. The runtime retains ownership until a later Stop
	// confirms termination, so a still-serving server is never dropped.
	retiredAPI []*api.Server

	// stopAPIServer terminates an API server and reports whether termination
	// was confirmed. Defaults to api.Server.Stop; a field so tests can
	// simulate a shutdown that never confirms termination.
	stopAPIServer func(*api.Server) bool

	// beforeGitOpen runs after the memory readers are pinned and immediately
	// before the git repository is opened, at the git-to-memory identity
	// boundary. A test stages a repoint of the active repository path in this
	// window so the git and memory identities differ and candidate
	// construction must fail closed. Nil on every production path.
	beforeGitOpen func()

	// enterApply, afterPrepare, and leaveApply are test seams for the apply
	// transaction. enterApply runs at the top of ApplyConfig under applyMu;
	// afterPrepare runs once a prepared candidate is ready and still
	// unpublished; leaveApply runs at the end of ApplyConfig under applyMu.
	enterApply   func()
	afterPrepare func()
	leaveApply   func()

	// beforeApplyMu runs at the top of setActiveAgent, immediately before it
	// acquires applyMu. Tests use it as a barrier to prove the active-agent
	// write reached the transaction lock. Nil on every production path.
	beforeApplyMu func()

	// shutdownHook records the step transitions of one shutdown attempt:
	// admissions-closed, root-cancelled, tasks-cancelled, sessions-flushed,
	// api-stopped, queue-wait, generation-released. Tests use it to assert the
	// shutdown lifecycle order and to block at a specific step. Nil on every
	// production path.
	shutdownHook func(step string)

	// afterProjectIdentity runs in EditProject once the repository identity
	// decision is settled and immediately before the workflow update executes.
	// Tests use it to repoint an alias in that window and prove the settled
	// decision carries through the mutation. Nil on every production path.
	afterProjectIdentity func()

	// flushMu serializes the shutdown session flush: at most one detached
	// FlushAll runs at a time, and a later Shutdown joins the in-flight flush
	// instead of stacking another, so blocked flushes cannot accumulate saveMu
	// waiters or produce duplicate durable saves. The result is published under
	// flushMu before the completion channel is closed, so no retry can miss the
	// completion; the channel is closed (broadcast), not signalled by a value.
	flushMu      sync.Mutex
	flushRunning bool
	flushDone    chan struct{}
	flushLastErr error
	flushEver    bool

	// beforeFlushPublish and afterFlushNotify are test seams for the detached
	// flush. beforeFlushPublish runs once FlushAll returns and before the
	// result is published; afterFlushNotify runs after the completion channel
	// is closed. Nil on every production path.
	beforeFlushPublish func()
	afterFlushNotify   func()
}

// New returns a runtime seeded with the loaded config and shared log rings.
func New(cfg config.Config, cfgStore config.Store, rings LogRings) *Runtime {
	return &Runtime{
		cfg:           cfg,
		cfgStore:      cfgStore,
		logRings:      rings,
		stopAPIServer: func(s *api.Server) bool { return s.Stop() },
	}
}

// NewEventChannel returns the process event channel shared by all managers.
func NewEventChannel() chan proc.Event {
	return make(chan proc.Event, EventBufferSize)
}

// AcquireRequestGeneration captures the current generation's assembler and
// session manager under the lock, increments the generation lease count,
// and returns static adapters bound to the captured concrete objects plus
// the captured active agent. The caller must use the returned adapters
// (not Runtime fields) and call release when the request completes.
func (rt *Runtime) AcquireRequestGeneration() (api.Assembler, api.SessionRecorder, string, func()) {
	rt.mu.Lock()
	g := rt.gen
	if g == nil {
		rt.mu.Unlock()
		return nil, nil, "", func() {}
	}
	g.acquire()
	asm := g.assembler
	mgr := g.sessionMgr
	active := rt.cfg.Agent.Active
	rt.mu.Unlock()

	var rec api.SessionRecorder
	if mgr != nil {
		rec = &staticSessionRecorder{mgr: mgr}
	}
	return &staticAssembler{asm: asm, active: active}, rec, active, g.release
}

// AcquireUISnapshot implements ui.SnapshotProvider. It atomically captures
// the current generation's complete UI dependency snapshot and pins the
// generation under rt.mu, so publication cannot retire and close the old
// generation between a handler selecting its snapshot and obtaining its
// lease. The caller must use only fields from the returned snapshot and call
// release when the request completes; release is safe to transfer to a
// detached goroutine.
func (rt *Runtime) AcquireUISnapshot() (ui.ServiceDeps, func()) {
	rt.mu.Lock()
	g := rt.gen
	if g == nil {
		rt.mu.Unlock()
		return ui.ServiceDeps{}, func() {}
	}
	g.acquire()
	snap := g.uiSnap
	// The active agent is a user selection that changes without a generation
	// rebuild (/agents/active), so it is resolved here per acquisition under
	// the same lock as the generation rather than frozen in the snapshot.
	snap.ActiveAgent = rt.cfg.Agent.Active
	rt.mu.Unlock()
	return snap, g.release
}

// staticAssembler implements api.Assembler against a concrete assembler
// captured from one generation. It does not reread Runtime fields.
type staticAssembler struct {
	asm    *prompt.DiskAssembler
	active string
}

func (a *staticAssembler) Assemble(ctx context.Context, agentName string, conversation []inference.Message) ([]inference.Message, error) {
	if agentName == "" {
		agentName = a.active
	}
	if agentName == "" {
		return nil, errNoActiveAgent
	}
	msgs, _, err := a.asm.Assemble(ctx, agentName, conversation)
	return msgs, err
}

// staticSessionRecorder implements api.SessionRecorder against a concrete
// session manager captured from one generation.
type staticSessionRecorder struct{ mgr *session.Manager }

func (r *staticSessionRecorder) Start(agentName string) api.Session {
	s := r.mgr.Start(agentName)
	return api.Session{ID: s.ID, Agent: s.Agent}
}

func (r *staticSessionRecorder) Append(id, role, content string) error {
	return r.mgr.Append(id, inference.Message{Role: role, Content: content})
}

func (r *staticSessionRecorder) Save(ctx context.Context, id string) error {
	_, err := r.mgr.Save(ctx, id)
	return err
}

func (r *staticSessionRecorder) End(id string) { r.mgr.End(id) }
