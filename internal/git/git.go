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
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/VrncQuentin/harness/internal/pathid"
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
// temporary directory and opening it.
type Repo struct {
	repo *gogit.Repository
	path string
	// id is the repository's physical identity, resolved once — immediately
	// after go-git opens the repository — rather than left for a caller to
	// re-resolve independently later. lockKey is its comparison key; Identity
	// exposes the full ID for callers that need to compare it against another
	// component's own identity rather than reopen the path a second time.
	id      pathid.ID
	lockKey string
	// dirInfo is the repository directory's FileInfo, captured immediately
	// after go-git opened it. pathid.ID is a canonical path spelling, not a
	// filesystem object: a directory renamed aside and replaced by a
	// different one under the same name resolves to an Equal ID before and
	// after, because Resolve reduces to path strings, never opens anything,
	// and has no notion of "the same object" at all. A caller that needs to
	// know two handles reached the same physical directory — not merely a
	// path that means the same thing right now — compares dirInfo with
	// os.SameFile instead. See DirInfo.
	dirInfo os.FileInfo
	mu      sync.Mutex
}

// repoMutationLocks serializes local mutations per repository.
//
// r.mu cannot do this job: a handle is opened per tool call, so two concurrent
// calls against the same repository hold two different mutexes and exclude
// nothing. The index, the refs, and the worktree are shared filesystem state,
// so the lock has to be keyed by the repository rather than by the handle.
//
// This guards harness-internal concurrency only. A user running git in the same
// repository at the same moment is outside its reach.
var repoMutationLocks sync.Map // lock key -> *sync.Mutex

// newRepo wraps an opened go-git repository, resolving its physical identity
// once so every handle on the same repository shares one mutation lock.
//
// Identity comes from internal/pathid rather than filepath.EvalSymlinks, which
// leaves a Windows junction unresolved: a junction alias and its target would
// hash to different keys, hand out two different mutexes, and leave concurrent
// writes to one repository completely unserialized.
//
// The resolution happens here, immediately after go-git has opened the
// repository, rather than being left to a caller to redo independently later.
// go-git gives no way to ask what it actually opened, so this is the closest
// approximation of that available: an identity taken as soon as possible after
// the fact, not a fresh resolution of the same path at some arbitrary later
// point that the repository's own physical location may have moved on from by
// then.
func newRepo(repo *gogit.Repository, path string) (*Repo, error) {
	id, err := pathid.Resolve(path)
	if err != nil {
		return nil, fmt.Errorf("git: identify repository %s: %w", path, err)
	}
	// Stat'd immediately after go-git's own open, for the same reason id is
	// resolved here rather than left to a caller: this is the closest
	// approximation available of "what go-git actually opened," since go-git
	// gives no way to ask and addresses its storage by path throughout.
	dirInfo, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("git: stat repository %s: %w", path, err)
	}
	return &Repo{repo: repo, path: path, id: id, lockKey: id.Key(), dirInfo: dirInfo}, nil
}

// Identity returns the physical identity resolved when this handle was
// opened, for a caller that needs to confirm another component — one it
// cannot bind to this handle directly — is looking at the same repository.
//
// This answers "what path does the other side currently mean," which is a
// weaker question than "did the other side open the same directory this
// handle did" — see DirInfo, which answers that one.
func (r *Repo) Identity() pathid.ID { return r.id }

// DirInfo returns the repository directory's FileInfo, captured immediately
// after go-git opened it. A caller comparing two independently-opened handles
// on what should be the same repository — this one and, say, a
// *memory.DirReader pinned on the same configured path — should compare their
// two DirInfo values with os.SameFile rather than their Identity values: two
// Identity values can be Equal while describing different physical
// directories if one was renamed aside and a different directory installed
// under the same name between the two opens, because pathid.ID reduces a
// path to its canonical string form and never inspects the object a path
// currently names. os.SameFile does inspect the object, which is what this
// question actually needs answered.
func (r *Repo) DirInfo() os.FileInfo { return r.dirInfo }

// lockRepo takes the repository-wide mutation lock and this handle's mutex, and
// returns the function that releases both in the right order.
//
// Every local mutation takes it — commit, branch creation, checkout — because
// they contend for the same state. Serializing commits against each other but
// not against a checkout would still let HEAD, the index, and the worktree move
// underneath a commit that is midway through.
//
// Read paths deliberately do not take it: go-git reads from disk each time, and
// blocking every status or log behind a write would serialize the whole tool
// surface for no correctness gain.
func (r *Repo) lockRepo() func() {
	actual, _ := repoMutationLocks.LoadOrStore(r.lockKey, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		// Unreachable: only *sync.Mutex is ever stored.
		lock = &sync.Mutex{}
	}
	lock.Lock()
	r.mu.Lock()
	return func() {
		r.mu.Unlock()
		lock.Unlock()
	}
}

// Open opens an existing git repository at path. It returns a wrapped
// error if the path does not exist or is not a git repository.
func Open(path string) (*Repo, error) {
	r, err := gogit.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	return newRepo(r, path)
}

// Init opens an existing plain git repository at path, or initializes a new
// one there when none exists yet. The directory is created if missing.
//
// The MkdirAll is a bootstrap: it creates the repository root that callers
// afterwards pin, and it is followed immediately by go-git, which addresses its
// storage by pathname and has no way to accept a directory handle. Routing this
// one call through a root would not change that, so identity is enforced where
// it can be — newRepo resolves the repository with pathid, and the C2 scope
// checks around the git tools do the same. See the filesystem access ledger in
// docs/architecture.md.
func Init(path string) (*Repo, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("git: create repo dir %s: %w", path, err)
	}
	r, err := gogit.PlainOpen(path)
	if err == nil {
		return newRepo(r, path)
	}
	if !errors.Is(err, gogit.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	r, err = gogit.PlainInit(path, false)
	if err != nil {
		return nil, fmt.Errorf("git: init %s: %w", path, err)
	}
	return newRepo(r, path)
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
	unlock := r.lockRepo()
	defer unlock()
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

// ErrDetachedHEAD reports that HEAD does not name a branch. It carries the
// short hash HEAD points at so a caller can say what it found.
type ErrDetachedHEAD struct {
	Short string
}

func (e *ErrDetachedHEAD) Error() string {
	return fmt.Sprintf("HEAD is detached at %s", e.Short)
}

// CurrentBranch returns the short name of the branch HEAD points at.
//
// It reads HEAD through the repository handle this Repo already holds, which is
// what removes the second pathname resolution the git_push tool used to do: that
// one validated the repository root and then opened root/.git/HEAD by name, so
// the path that was authorized and the path that was read were resolved
// separately, and it broke outright on a linked worktree, where .git is a file
// rather than a directory.
//
// What this does not provide is a handle guarantee. go-git addresses its
// storage by pathname internally, so the read is still ultimately by name — the
// improvement is that there is one resolution instead of two, performed by the
// component that owns the repository, and that it understands the layouts a
// hand-rolled read of .git/HEAD does not. Repository opening keeps the explicit
// pathid identity and C2 checks around it for exactly this reason.
//
// HEAD is read unresolved, so a repository with no commits yet still reports its
// branch instead of failing on a reference that points at nothing.
func (r *Repo) CurrentBranch() (string, error) {
	ref, err := r.repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return "", fmt.Errorf("git: read HEAD in %s: %w", r.path, err)
	}
	if ref.Type() != plumbing.SymbolicReference {
		short := ref.Hash().String()
		if len(short) > 8 {
			short = short[:8]
		}
		return "", &ErrDetachedHEAD{Short: short}
	}
	target := ref.Target()
	if !target.IsBranch() {
		return "", &ErrDetachedHEAD{Short: target.String()}
	}
	return target.Short(), nil
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
