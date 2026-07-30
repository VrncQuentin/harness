// Package runtime owns the mutable service graph behind the harness.
package runtime

import (
	"errors"
	"sync"

	"github.com/VrncQuentin/harness/internal/agent"
	"github.com/VrncQuentin/harness/internal/api"
	"github.com/VrncQuentin/harness/internal/config"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/logbuf"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/proc"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/prompt"
	"github.com/VrncQuentin/harness/internal/queue"
	"github.com/VrncQuentin/harness/internal/session"
)

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
	// apiSessionReader holds the previous session reader when the API
	// was carried forward across a same-config reload. It is closed
	// on Stop or when the API is eventually rebuilt.
	apiSessionReader memory.Repo
	agentReg         *agent.DiskRegistry
	assembler        *prompt.DiskAssembler
	apiServer        *api.Server
	gitRepo          *gitw.Repo
	sessionMu        sync.RWMutex
	sessionMg        *session.Manager
	taskRunner       *taskRunnerAdapter
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
