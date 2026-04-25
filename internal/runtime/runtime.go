// Package runtime owns the mutable service graph behind the harness.
package runtime

import (
	"errors"
	"sync"

	"github.com/vrnc/harness/internal/agent"
	"github.com/vrnc/harness/internal/api"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/logbuf"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/proc"
	"github.com/vrnc/harness/internal/prompt"
	"github.com/vrnc/harness/internal/queue"
)

// ErrConfigStoreUnavailable is surfaced when the harness DB could not be
// opened, so the user sees one consistent message in the status page and the
// config editor.
var ErrConfigStoreUnavailable = errors.New("config store unavailable (harness.db could not be opened)")

// EventBufferSize absorbs process-manager startup bursts without forcing
// managers to block while the UI event forwarder catches up.
const EventBufferSize = 64

// Rings groups the in-memory log buffers shared by services and the UI.
type Rings struct {
	Log   *logbuf.Ring
	Llama *logbuf.Ring
	Embed *logbuf.Ring
}

// Runtime holds mutable service references that the retry/save callback
// reconfigures in place. A mutex guards all fields because callbacks run on
// HTTP goroutines while event forwarding and metrics read managers and queue.
type Runtime struct {
	mu       sync.Mutex
	cfg      config.Config
	cfgStore config.Store
	rings    Rings

	llamaMgr *proc.Manager
	embedMgr *proc.Manager
	reqQueue *queue.Queue
	started  bool

	memReader *memory.DirReader
	agentReg  *agent.DiskRegistry
	assembler *prompt.DiskAssembler
	hotReload *prompt.HotReload
	apiServer *api.Server
}

// New returns a runtime seeded with the loaded config and shared log rings.
func New(cfg config.Config, cfgStore config.Store, rings Rings) *Runtime {
	return &Runtime{
		cfg:      cfg,
		cfgStore: cfgStore,
		rings:    rings,
	}
}

// NewEventChannel returns the process event channel shared by all managers.
func NewEventChannel() chan proc.Event {
	return make(chan proc.Event, EventBufferSize)
}
