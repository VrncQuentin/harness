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
)

// generation owns the concrete resources of one reload cycle: readers,
// assembler, and session manager. Operations acquire a lease before using
// any generation resource and release it after. When the lease count
// reaches zero, the generation's readers and owned handles are closed.
type generation struct {
	readers    []memory.Repo
	assembler  *prompt.DiskAssembler
	sessionMgr *session.Manager
	handles    []io.Closer
	leases     atomic.Int64
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
	sessionMem memory.Repo // session store DirReader, closed on Stop
	agentReg   *agent.DiskRegistry
	assembler  *prompt.DiskAssembler
	apiServer  *api.Server
	sessionMu  sync.RWMutex
	sessionMg  *session.Manager
	taskRunner *taskRunnerAdapter
	gen        *generation

	// beforeGitOpen runs after the memory readers are pinned and immediately
	// before the git repository is opened, at the git-to-memory identity
	// boundary. A test stages a repoint of the active repository path in this
	// window so the git and memory identities differ and candidate
	// construction must fail closed. Nil on every production path.
	beforeGitOpen func()
}

// New returns a runtime seeded with the loaded config and shared log rings.
func New(cfg config.Config, cfgStore config.Store, rings LogRings) *Runtime {
	return &Runtime{
		cfg:      cfg,
		cfgStore: cfgStore,
		logRings: rings,
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
