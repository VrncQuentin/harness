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
	"slices"
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

// errNoFileWriter is returned by WritePersona/WriteNotes when the
// underlying Reader does not implement memory.FileWriter. Mirrors
// errNoDirCreator: production always satisfies it; the explicit
// error keeps test fakes that forget the capability discoverable.
var errNoFileWriter = errors.New("agent: memory.Reader must also implement memory.FileWriter")

// errNoDirRemover is returned by Delete when the underlying Reader
// does not implement memory.DirRemover. Same shape as the writer/
// creator counterparts above.
var errNoDirRemover = errors.New("agent: memory.Reader must also implement memory.DirRemover")

// ErrInvalidName is returned by Create when the requested agent name
// fails validation (empty, too long, or contains disallowed
// characters). Wrapped so callers can use errors.Is.
var ErrInvalidName = errors.New("agent: invalid name")

// ErrAgentExists is returned by Create when the agents/<name>/
// directory already exists. Wrapped so callers can use errors.Is to
// distinguish a duplicate from other failures.
var ErrAgentExists = errors.New("agent: already exists")

// Agent is the minimal metadata the prompt assembler needs: the agent's
// name and the repo-relative paths of its persona, rules, and notes
// files. Rules are an optional per-agent layer analogous to
// global/rules.md - always-on behavioural constraints scoped to this
// agent (e.g. "make a plan before any edit").
type Agent struct {
	Name        string
	PersonaPath string
	RulesPath   string
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
	// WritePersona replaces agents/<name>/persona.md with body. The
	// agent must already exist; an unknown name returns an error
	// wrapping fs.ErrNotExist.
	WritePersona(name string, body []byte) error
	// WriteRules replaces agents/<name>/rules.md with body. Same
	// rules as WritePersona.
	WriteRules(name string, body []byte) error
	// WriteNotes replaces agents/<name>/notes.md with body. Same
	// rules as WritePersona.
	WriteNotes(name string, body []byte) error
	// Delete removes agents/<name>/ and every file under it. The
	// active agent is cleared before removal if it matched name so
	// the prompt assembler is never left pointing at a vanished
	// directory. Unknown agents return an error wrapping
	// fs.ErrNotExist.
	Delete(name string) error
}

// DiskRegistry lists agents by scanning agents/ in the memory repo.
// The active agent lives in the config row; this type holds no
// persistent state of its own, it delegates via callbacks passed at
// construction time.
type DiskRegistry struct {
	lister    memory.DirLister
	creator   memory.DirCreator
	writer    memory.FileWriter
	remover   memory.DirRemover
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
// mem must also implement memory.DirLister, memory.DirCreator, and
// memory.FileWriter (the production *memory.DirReader does). If it
// does not, the affected calls return an error - this trades a
// startup panic for a deterministic failure mode that the UI can
// render.
func NewDiskRegistry(mem memory.Reader, getActive func() string, setActive func(string) error) *DiskRegistry {
	dl, _ := mem.(memory.DirLister)
	dc, _ := mem.(memory.DirCreator)
	fw, _ := mem.(memory.FileWriter)
	dr, _ := mem.(memory.DirRemover)
	return &DiskRegistry{
		lister:    dl,
		creator:   dc,
		writer:    fw,
		remover:   dr,
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
	if slices.Contains(names, name) {
		return newAgent(name), nil
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
		RulesPath:   path.Join(agentsDir, name, "rules.md"),
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
	if slices.Contains(existing, name) {
		return Agent{}, fmt.Errorf("agent: %q: %w", name, ErrAgentExists)
	}

	if err := r.creator.MkdirAll(path.Join(agentsDir, name)); err != nil {
		return Agent{}, fmt.Errorf("agent: create %q: %w", name, err)
	}
	return newAgent(name), nil
}

// WritePersona replaces the agent's persona.md with body. The file is
// created if missing. Errors from validation, lookup, or the
// underlying writer are wrapped with package context.
func (r *DiskRegistry) WritePersona(name string, body []byte) error {
	a, err := r.resolveForWrite(name)
	if err != nil {
		return err
	}
	if err := r.writer.WriteFile(a.PersonaPath, body); err != nil {
		return fmt.Errorf("agent: write persona %q: %w", name, err)
	}
	return nil
}

// WriteRules replaces the agent's rules.md with body. The file is
// created if missing.
func (r *DiskRegistry) WriteRules(name string, body []byte) error {
	a, err := r.resolveForWrite(name)
	if err != nil {
		return err
	}
	if err := r.writer.WriteFile(a.RulesPath, body); err != nil {
		return fmt.Errorf("agent: write rules %q: %w", name, err)
	}
	return nil
}

// WriteNotes replaces the agent's notes.md with body. The file is
// created if missing.
func (r *DiskRegistry) WriteNotes(name string, body []byte) error {
	a, err := r.resolveForWrite(name)
	if err != nil {
		return err
	}
	if err := r.writer.WriteFile(a.NotesPath, body); err != nil {
		return fmt.Errorf("agent: write notes %q: %w", name, err)
	}
	return nil
}

// Delete removes agents/<name>/ and any files under it. The name is
// validated so a malformed string can never escape the agents/ root.
// If the requested agent is currently active, the active selection
// is cleared first so the prompt assembler and hot-reload watcher
// are never left pointing at a vanished directory.
func (r *DiskRegistry) Delete(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if r.lister == nil {
		return errNoDirLister
	}
	if r.remover == nil {
		return errNoDirRemover
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, err := r.lister.ListDirs(agentsDir)
	if err != nil {
		return fmt.Errorf("agent: delete %q: %w", name, err)
	}
	if !slices.Contains(existing, name) {
		return fmt.Errorf("agent: %q: %w", name, fs.ErrNotExist)
	}

	// Clear active before removing the directory so anything that
	// observes the active value (prompt assembler, hot-reload
	// watcher) sees a consistent state - never "active points at a
	// missing folder". The callbacks are optional in test setups, so
	// only act when both are wired.
	if r.getActive != nil && r.setActive != nil && r.getActive() == name {
		if err := r.setActive(""); err != nil {
			return fmt.Errorf("agent: delete %q: clear active: %w", name, err)
		}
	}

	if err := r.remover.RemoveAll(path.Join(agentsDir, name)); err != nil {
		return fmt.Errorf("agent: delete %q: %w", name, err)
	}
	return nil
}

// resolveForWrite verifies the writer is wired and the named agent
// exists, returning the metadata used to derive on-disk paths.
func (r *DiskRegistry) resolveForWrite(name string) (Agent, error) {
	if name == "" {
		return Agent{}, fmt.Errorf("agent: name is empty")
	}
	if r.writer == nil {
		return Agent{}, errNoFileWriter
	}
	a, err := r.Get(name)
	if err != nil {
		return Agent{}, err
	}
	return a, nil
}

// ValidateName enforces the same conservative agent-name policy used
// by the registry. It returns an error wrapping ErrInvalidName when
// name fails validation.
func ValidateName(name string) error {
	return validateName(name)
}

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
