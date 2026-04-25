// Package agent owns the registry of named agents. An agent is simply
// a subdirectory under agents/ in the memory repo containing at least
// a persona.md; the registry enumerates them, exposes their well-known
// file paths to the prompt assembler, and tracks which one is active.
package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sync"

	"github.com/vrnc/harness/internal/memory"
)

// agentsDir is the root of the agent subtree inside the memory repo.
const agentsDir = "agents"

// errNoDirLister is returned when the Reader passed to NewDiskRegistry
// does not also implement memory.DirLister. The production
// *memory.DirReader always satisfies both.
var errNoDirLister = errors.New("agent: memory.Reader must also implement memory.DirLister")

// Agent is the minimal metadata the prompt assembler needs: the agent's
// name and the repo-relative paths of its persona and notes files.
type Agent struct {
	Name        string
	PersonaPath string
	NotesPath   string
}

// Registry is the interface the UI and prompt assembler use to pick and
// switch agents. Implementations must be safe for concurrent use.
type Registry interface {
	List() ([]Agent, error)
	Get(name string) (Agent, error)
	Active() string
	SetActive(name string) error
}

// DiskRegistry lists agents by scanning agents/ in the memory repo.
// The active agent lives in the config row; this type holds no
// persistent state of its own, it delegates via callbacks passed at
// construction time.
type DiskRegistry struct {
	lister    memory.DirLister
	mu        sync.Mutex
	getActive func() string
	setActive func(string) error
}

var _ Registry = (*DiskRegistry)(nil)

// NewDiskRegistry builds a registry backed by mem. The getActive and
// setActive callbacks read and write the active agent name; the
// registry is agnostic to where that value is stored (in practice,
// internal/db's config row).
//
// mem must also implement memory.DirLister (the production
// *memory.DirReader does). If it does not, every subsequent List/Get
// call returns an error - this trades a startup panic for a
// deterministic failure mode that the UI can render.
func NewDiskRegistry(mem memory.Reader, getActive func() string, setActive func(string) error) *DiskRegistry {
	dl, _ := mem.(memory.DirLister)
	return &DiskRegistry{
		lister:    dl,
		getActive: getActive,
		setActive: setActive,
	}
}

// List returns every subdirectory under agents/ sorted by name. A
// missing agents/ directory yields an empty slice rather than an
// error so a freshly-initialised memory repo is not surfaced as a
// fatal setup problem.
func (r *DiskRegistry) List() ([]Agent, error) {
	if r.lister == nil {
		return nil, errNoDirLister
	}
	names, err := r.lister.ListDirs(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("agent: list: %w", err)
	}
	out := make([]Agent, 0, len(names))
	for _, n := range names {
		out = append(out, newAgent(n))
	}
	return out, nil
}

// Get returns the agent named name. Unknown agents surface as an error
// wrapping fs.ErrNotExist so callers can use errors.Is for "missing
// agent" checks.
func (r *DiskRegistry) Get(name string) (Agent, error) {
	if name == "" {
		return Agent{}, fmt.Errorf("agent: name is empty")
	}
	if r.lister == nil {
		return Agent{}, errNoDirLister
	}
	names, err := r.lister.ListDirs(agentsDir)
	if err != nil {
		return Agent{}, fmt.Errorf("agent: get %q: %w", name, err)
	}
	for _, n := range names {
		if n == name {
			return newAgent(name), nil
		}
	}
	return Agent{}, fmt.Errorf("agent: %q: %w", name, fs.ErrNotExist)
}

// Active returns the currently active agent name. Empty string means
// no agent is active (valid state; the prompt assembler skips the
// persona layers entirely).
func (r *DiskRegistry) Active() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getActive == nil {
		return ""
	}
	return r.getActive()
}

// SetActive changes the active agent. The empty string is accepted and
// clears the active agent; any other name must match an existing
// subdirectory under agents/.
func (r *DiskRegistry) SetActive(name string) error {
	if name != "" {
		if _, err := r.Get(name); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.setActive == nil {
		return errors.New("agent: SetActive callback not configured")
	}
	if err := r.setActive(name); err != nil {
		return fmt.Errorf("agent: persist active %q: %w", name, err)
	}
	return nil
}

// newAgent returns the Agent metadata for name without touching disk;
// callers have already verified the directory exists.
func newAgent(name string) Agent {
	return Agent{
		Name:        name,
		PersonaPath: path.Join(agentsDir, name, "persona.md"),
		NotesPath:   path.Join(agentsDir, name, "notes.md"),
	}
}
