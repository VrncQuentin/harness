// Package git wraps github.com/go-git/go-git/v6 for local harness repos.
// It exposes a small surface to open or initialize a repository and commit
// specific files. Project memory repos are initialized by the harness through
// Init; cloning remains outside this package.
package git

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/VrncQuentin/harness/internal/coord"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// defaultAuthorName and defaultAuthorEmail are used when the repo's git
// config does not provide user.name / user.email. Harness commits run on
// behalf of the harness itself, so a stable fallback identity keeps the
// log consistent without forcing the user to configure git first.
const (
	defaultAuthorName  = "harness"
	defaultAuthorEmail = "harness@local"
)

// Repo is a thin handle over an opened go-git repository. Callers obtain
// it via Open; tests construct one by initialising a fresh repo on a
// temporary directory and opening it. Callers close the handle with Close
// when done.
//
// Identity: the wrapper pins and verifies its repository boundary when the
// handle is opened and retains the pinned handle. Comparisons against other
// components' opened boundaries use os.SameFile on the retained handles, so a
// directory replaced at the same pathname is detected. go-git itself opens
// and reads storage by pathname — it is not handle-relative — so a pathname
// repointed after the open is a documented go-git limitation, not something
// this wrapper claims to defend against.
type Repo struct {
	repo *gogit.Repository
	path string
	// boundary is the repository directory pinned at open time, retained so
	// identity comparison against another component's opened boundary is a
	// SameFile comparison on the objects actually pinned rather than a
	// pathname re-resolution. It is private: callers cannot close it.
	boundary *rootfs.Anchor
	// identity is the repository's physical pathid, resolved once when the
	// handle is opened and verified against the pinned boundary. It selects
	// the repository-wide mutation coordinator, so two handles on the same
	// repository must produce the same identity however each was spelled.
	identity pathid.ID
	// gate is the repository-wide mutation coordinator shared with index
	// publication. It is the same gate index handles on this repository
	// acquire, so git mutations and index publications serialize on one object.
	gate *coord.Gate
	mu   sync.Mutex
}

// newRepo wraps an opened go-git repository, resolving its physical identity
// once so every handle on the same repository shares one mutation
// coordinator.
//
// Identity comes from internal/pathid rather than filepath.EvalSymlinks, which
// leaves a Windows junction unresolved: a junction alias and its target would
// hash to different keys, hand out two different coordinators, and leave
// concurrent writes to one repository completely unserialized.
//
// The boundary is pinned and verified with rootfs.OpenIdentifiedHooked before
// the identity is accepted, so a pathname that moved between the go-git open
// and the identity resolution fails closed rather than silently identifying
// the replacement. The pinned handle is retained as the boundary evidence;
// Close releases it.
func newRepo(repo *gogit.Repository, path string, afterPin func()) (*Repo, error) {
	pinned, id, err := rootfs.OpenIdentifiedHooked(path, afterPin)
	if err != nil {
		return nil, fmt.Errorf("git: identify repository %s: %w", path, err)
	}
	return &Repo{
		repo:     repo,
		path:     path,
		boundary: rootfs.NewAnchorFromRoot(pinned, path),
		identity: id,
		gate:     coord.Default().GateFor(id.Key()),
	}, nil
}

// Close releases the pinned repository boundary. go-git operations do not use
// the retained handle and remain usable after Close; only identity comparison
// (SameAnchor) is unavailable. Closing a nil or already-closed handle is a
// no-op.
func (r *Repo) Close() error {
	if r == nil || r.boundary == nil {
		return nil
	}
	return r.boundary.Close()
}

// SameAnchor reports whether the repository directory this handle pinned is
// the same filesystem object as other. The comparison uses os.SameFile on the
// two retained pinned handles — no pathname re-resolution is involved — so a
// directory replaced at the same pathname between the two opens is reported
// as different.
func (r *Repo) SameAnchor(other *rootfs.Anchor) (bool, error) {
	return r.boundary.SameAnchor(other)
}

// Identity returns the verified physical pathid of the repository directory
// this handle opened. It is retained from open time and bound to the pinned
// boundary. The pathid alone does not preserve opened-object identity — a
// same-name replacement at the pathname reuses the same key — so compare
// opened boundaries with SameAnchor, not with this value.
func (r *Repo) Identity() pathid.ID { return r.identity }

// WithMutation runs fn under the repository-wide mutation coordinator,
// holding it for the whole call. Index publication and the following git
// commit for one repository must happen inside one WithMutation call so they
// are one in-process mutation transaction: no other git mutation or index
// publication on the repository can interleave between them.
//
// fn receives a *Mutation whose commit methods operate without reacquiring
// the coordinator. Calling the *Repo* methods inside fn would deadlock; use
// the Mutation methods instead. Index publication joins the transaction
// through Index.UpsertRootedUnder with m.Gate(), which asserts the gate this
// transaction holds is the index's own.
func (r *Repo) WithMutation(fn func(*Mutation) error) error {
	return r.withMutation(fn, nil)
}

// WithMutationHooked is WithMutation with a hook that runs immediately before
// the repository gate is acquired. It is exported so regression tests in other
// packages can stage the interleaving at the real acquisition boundary;
// production callers use WithMutation, which passes no hook.
func (r *Repo) WithMutationHooked(fn func(*Mutation) error, beforeGate func()) error {
	return r.withMutation(fn, beforeGate)
}

// withMutation is WithMutation with a hook that runs immediately before the
// repository gate is acquired. The hook is a parameter rather than package
// state so parallel tests cannot see each other's; it is nil on every
// production path. A test uses it to prove a contender reached the
// acquisition point before asserting it is blocked on the gate.
func (r *Repo) withMutation(fn func(*Mutation) error, beforeGate func()) error {
	if beforeGate != nil {
		beforeGate()
	}
	r.gate.Lock()
	defer r.gate.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(&Mutation{r: r})
}

// Mutation is a repository mutation session. The coordinator is already held
// by the WithMutation call that produced it; every method on it operates
// without reacquiring it.
type Mutation struct {
	r *Repo
}

// Gate returns the coordinator this session holds. Components that share the
// repository (index publication) use it to join the transaction without
// acquiring a second gate.
func (m *Mutation) Gate() *coord.Gate { return m.r.gate }

// Open opens an existing git repository at path. It returns a wrapped
// error if the path does not exist or is not a git repository.
func Open(path string) (*Repo, error) {
	return open(path, nil)
}

// open is Open with a hook that runs after go-git opens the repository and
// after the repository boundary is pinned, in the window before its identity
// is resolved. The hook is a parameter rather than package state so parallel
// tests cannot see each other's; it is nil on every production path.
func open(path string, afterPin func()) (*Repo, error) {
	r, err := gogit.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	return newRepo(r, path, afterPin)
}

// Init opens an existing plain git repository at path, or initializes a new
// one there when none exists yet. The directory is created if missing.
func Init(path string) (*Repo, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("git: create repo dir %s: %w", path, err)
	}
	r, err := gogit.PlainOpen(path)
	if err == nil {
		return newRepo(r, path, nil)
	}
	if !errors.Is(err, gogit.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	r, err = gogit.PlainInit(path, false)
	if err != nil {
		return nil, fmt.Errorf("git: init %s: %w", path, err)
	}
	return newRepo(r, path, nil)
}

// Commit stages each file in files (paths relative to the repo root,
// using forward slashes) and creates a commit with msg as the message.
//
// Caller contract: files must already exist in the working tree on disk
// before Commit is called. go-git's Worktree.Add reads the working tree
// to compute the staged blob, so the bytes need to land first.
//
// Author identity is read from the repo's git config; if either name or
// email is missing, the harness defaults are used.
//
// The returned sha is the new commit's hex SHA.
func (r *Repo) Commit(msg string, files []string) (string, error) {
	var sha string
	err := r.WithMutation(func(m *Mutation) error {
		var err error
		sha, err = m.Commit(msg, files)
		return err
	})
	return sha, err
}

// Commit stages and commits within a held repository mutation session.
func (m *Mutation) Commit(msg string, files []string) (string, error) {
	return m.r.commitLocked(msg, files)
}

func (r *Repo) commitLocked(msg string, files []string) (string, error) {
	wt, err := r.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("git: worktree %s: %w", r.path, err)
	}
	for _, f := range files {
		if _, err := wt.Add(f); err != nil {
			return "", fmt.Errorf("git: add %s: %w", f, err)
		}
	}
	name, email := r.authorIdentity()
	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  name,
			Email: email,
			When:  time.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("git: commit %s: %w", r.path, err)
	}
	return hash.String(), nil
}

// authorIdentity returns the user.name / user.email pair from the repo's
// merged git config, falling back to the harness defaults when either
// value is unset or empty.
func (r *Repo) authorIdentity() (string, string) {
	name, email := defaultAuthorName, defaultAuthorEmail
	cfg, err := r.repo.ConfigScoped(0) // 0 = ConfigScopeSystem; merged view.
	if err != nil {
		return name, email
	}
	if cfg.User.Name != "" {
		name = cfg.User.Name
	}
	if cfg.User.Email != "" {
		email = cfg.User.Email
	}
	return name, email
}
