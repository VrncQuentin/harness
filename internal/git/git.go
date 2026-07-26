// Package git wraps github.com/go-git/go-git/v6 for local harness repos.
// It exposes a small surface to open or initialize a repository and commit
// specific files. Project memory repos are initialized by the harness through
// Init; cloning remains outside this package.
package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
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
	mu   sync.Mutex
}

// repoWriteLocks serializes multi-step writes per repository path.
//
// r.mu cannot do this job: a handle is opened per tool call, so two
// concurrent calls against the same repository hold two different mutexes and
// exclude nothing. Index and ref updates are shared filesystem state, so the
// lock has to be keyed by the repository rather than by the handle.
//
// This guards harness-internal concurrency only. A user running git in the
// same repository at the same time is outside its reach.
var repoWriteLocks sync.Map // lock key -> *sync.Mutex

// writeLock returns the mutex guarding writes to r's repository.
func (r *Repo) writeLock() *sync.Mutex {
	key := filepath.Clean(r.path)
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	actual, _ := repoWriteLocks.LoadOrStore(key, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		// Unreachable: only *sync.Mutex is ever stored.
		return &sync.Mutex{}
	}
	return lock
}

// Open opens an existing git repository at path. It returns a wrapped
// error if the path does not exist or is not a git repository.
func Open(path string) (*Repo, error) {
	r, err := gogit.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	return &Repo{repo: r, path: path}, nil
}

// Init opens an existing plain git repository at path, or initializes a new
// one there when none exists yet. The directory is created if missing.
func Init(path string) (*Repo, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("git: create repo dir %s: %w", path, err)
	}
	r, err := gogit.PlainOpen(path)
	if err == nil {
		return &Repo{repo: r, path: path}, nil
	}
	if !errors.Is(err, gogit.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("git: open %s: %w", path, err)
	}
	r, err = gogit.PlainInit(path, false)
	if err != nil {
		return nil, fmt.Errorf("git: init %s: %w", path, err)
	}
	return &Repo{repo: r, path: path}, nil
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
