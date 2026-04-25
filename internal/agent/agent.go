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
	"strings"
	"sync"

	"github.com/vrnc/harness/internal/memory"
)

// agentsDir is the root of the agent subtree inside the memory repo.
const agentsDir = "agents"

// maxAgentNameLen caps the agent name to keep the path within typical
// filesystem limits and the UI list readable.
const maxAgentNameLen = 64

// errNoDirLister is returned when the Reader passed to NewDiskRegistry
// does not also implement memory.DirLister. The production
// *memory.DirReader always satisfies both.
var errNoDirLister = errors.New("agent: memory.Reader must also implement memory.DirLister")

// errNoDirCreator is returned by Create when the underlying Reader does
// not implement memory.DirCreator. The production *memory.DirReader
// always satisfies it; this surfaces as a clear UI error if some
// future test fake forgets the capability.
var errNoDirCreator = errors.New("agent: memory.Reader must also implement memory.DirCreator")

// ErrInvalidName is returned by Create when the requested agent name
// fails validation (empty, too long, or contains disallowed
// characters). Wrapped so callers can use errors.Is.
var ErrInvalidName = errors.New("agent: invalid name")

// ErrAgentExists is returned by Create when the agents/<name>/
// directory already exists. Wrapped so callers can use errors.Is to
// distinguish a duplicate from other failures.
var ErrAgentExists = errors.New("agent: already exists")

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
	Create(name string) (Agent, error)
}

// DiskRegistry lists agents by scanning agents/ in the memory repo.
// The active agent lives in the config row; this type holds no
// persistent state of its own, it delegates via callbacks passed at
// construction time.
type DiskRegistry struct {
	lister    memory.DirLister
	creator   memory.DirCreator
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
// mem must also implement memory.DirLister and memory.DirCreator (the
// production *memory.DirReader does). If it does not, the affected
// calls return an error - this trades a startup panic for a
// deterministic failure mode that the UI can render.
func NewDiskRegistry(mem memory.Reader, getActive func() string, setActive func(string) error) *DiskRegistry {
	dl, _ := mem.(memory.DirLister)
	dc, _ := mem.(memory.DirCreator)
	return &DiskRegistry{
		lister:    dl,
		creator:   dc,
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

// Create makes a new agent named name by creating the agents/<name>/
// directory in the memory repo. The name is validated for length and
// allowed characters. If a directory by that name already exists,
// ErrAgentExists is returned and nothing is written.
//
// The new agent's persona.md is intentionally not seeded here - the
// user is expected to author it (or copy one in) before activating
// the agent. Until then the /agents page renders the persona card as
// "Persona file is empty or missing.", which is a clearer prompt than
// an empty stub.
func (r *DiskRegistry) Create(name string) (Agent, error) {
	if err := validateName(name); err != nil {
		return Agent{}, err
	}
	if r.lister == nil {
		return Agent{}, errNoDirLister
	}
	if r.creator == nil {
		return Agent{}, errNoDirCreator
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, err := r.lister.ListDirs(agentsDir)
	if err != nil {
		return Agent{}, fmt.Errorf("agent: create %q: %w", name, err)
	}
	for _, n := range existing {
		if n == name {
			return Agent{}, fmt.Errorf("agent: %q: %w", name, ErrAgentExists)
		}
	}

	if err := r.creator.MkdirAll(path.Join(agentsDir, name)); err != nil {
		return Agent{}, fmt.Errorf("agent: create %q: %w", name, err)
	}
	return newAgent(name), nil
}

// validateName enforces a conservative agent-name policy: 1-64 chars
// from [A-Za-z0-9._-], no leading dot or dash, and no reserved
// single-segment names like "." or "..". The character set is the
// safe intersection of POSIX and Windows directory rules.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidName)
	}
	if len(name) > maxAgentNameLen {
		return fmt.Errorf("%w: name longer than %d chars", ErrInvalidName, maxAgentNameLen)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidName, name)
	}
	if name[0] == '.' || name[0] == '-' {
		return fmt.Errorf("%w: name may not start with %q", ErrInvalidName, string(name[0]))
	}
	for _, c := range name {
		if !isNameChar(c) {
			return fmt.Errorf("%w: character %q not allowed", ErrInvalidName, string(c))
		}
	}
	// Belt and braces: rule out path separators that snuck through any
	// single-character classifier above.
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: path separators not allowed", ErrInvalidName)
	}
	return nil
}

// isNameChar reports whether c is one of the safe agent-name
// characters: ASCII letters, digits, dot, underscore, or hyphen.
func isNameChar(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '.' || c == '_' || c == '-':
		return true
	}
	return false
}
