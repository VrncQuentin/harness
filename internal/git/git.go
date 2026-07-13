// Package git wraps github.com/go-git/go-git/v5 for the harness memory
// repo. It exposes a small surface over an already-existing repo: open,
// commit specific files, walk the log filtered by structured tags, and
// fetch the bytes of a single-file commit.
//
// The package never initializes or clones a repo. Creating the memory
// repo is the user's responsibility - see the "Memory repo is never
// auto-created" decision in docs/architecture.md.
package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// defaultAuthorName and defaultAuthorEmail are used when the repo's git
// config does not provide user.name / user.email. Track C commits run on
// behalf of the harness itself, so a stable fallback identity keeps the
// log consistent without forcing the user to configure git first.
const (
	defaultAuthorName  = "harness"
	defaultAuthorEmail = "harness@local"
)

// CommitMeta describes a single commit returned by Log. Tags is the
// parsed result of the structured "[k:v]" prefix on the commit message;
// see ParseMessage. Tags is never nil - an absence of tags is signalled
// by an empty (non-nil) map.
type CommitMeta struct {
	SHA     string
	Author  string
	Time    time.Time
	Message string
	Tags    map[string]string
}

// Repo is a thin handle over an opened go-git repository. Callers obtain
// it via Open; tests construct one by initialising a fresh repo on a
// temporary directory and opening it.
type Repo struct {
	repo *gogit.Repository
	path string
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

// Log walks HEAD's history newest-first and returns commits whose parsed
// tags are a superset of filters. An empty or nil filters map returns
// every commit on HEAD. Each returned CommitMeta has its Tags populated
// from the leading "[k:v]" run on the commit message; the remainder is
// the Message (already trimmed of the tag prefix).
func (r *Repo) Log(filters map[string]string) ([]CommitMeta, error) {
	head, err := r.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git: head %s: %w", r.path, err)
	}
	iter, err := r.repo.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		return nil, fmt.Errorf("git: log %s: %w", r.path, err)
	}
	defer iter.Close()

	var out []CommitMeta
	err = iter.ForEach(func(c *object.Commit) error {
		tags, summary := ParseMessage(c.Message)
		if !tagsMatch(tags, filters) {
			return nil
		}
		out = append(out, CommitMeta{
			SHA:     c.Hash.String(),
			Author:  c.Author.Name,
			Time:    c.Author.When,
			Message: summary,
			Tags:    tags,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("git: iterate log: %w", err)
	}
	return out, nil
}

// tagsMatch reports whether got is a superset of want. When want is empty
// every commit matches.
func tagsMatch(got, want map[string]string) bool {
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			return false
		}
	}
	return true
}

// BlobByRef returns the bytes of the first file changed in the commit
// identified by sha.
//
// Constraint: M3 commits one file per commit (one episode per session),
// so "first changed file" is unambiguous in practice. Callers that need
// to inspect multi-file commits must use go-git directly. If the commit
// has no changes (an empty merge or otherwise), a clear error is
// returned.
func (r *Repo) BlobByRef(sha string) ([]byte, error) {
	hash := plumbing.NewHash(sha)
	commit, err := r.repo.CommitObject(hash)
	if err != nil {
		return nil, fmt.Errorf("git: commit %s: %w", sha, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("git: tree for %s: %w", sha, err)
	}

	parentTree, err := parentTreeOrNil(commit)
	if err != nil {
		return nil, fmt.Errorf("git: parent tree for %s: %w", sha, err)
	}

	changes, err := object.DiffTree(parentTree, tree)
	if err != nil {
		return nil, fmt.Errorf("git: diff for %s: %w", sha, err)
	}
	if len(changes) == 0 {
		return nil, fmt.Errorf("git: commit %s has no changes", sha)
	}

	// Sort by destination path so "first" is deterministic across
	// implementations of go-git's diff walk. Fall back to the source path
	// when the change is a deletion (unlikely for episodes but safe).
	sort.SliceStable(changes, func(i, j int) bool {
		return changePath(changes[i]) < changePath(changes[j])
	})

	change := changes[0]
	_, to, err := change.Files()
	if err != nil {
		return nil, fmt.Errorf("git: change files for %s: %w", sha, err)
	}
	if to == nil {
		return nil, fmt.Errorf("git: commit %s only deletes files", sha)
	}
	reader, err := to.Reader()
	if err != nil {
		return nil, fmt.Errorf("git: blob reader for %s: %w", sha, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("git: read blob for %s: %w", sha, err)
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("git: close blob reader for %s: %w", sha, err)
	}
	return data, nil
}

// parentTreeOrNil returns the tree of c's first parent, or nil for an
// initial commit (which has no parent and therefore no diff base).
func parentTreeOrNil(c *object.Commit) (*object.Tree, error) {
	if c.NumParents() == 0 {
		return nil, nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	return parent.Tree()
}

// changePath returns the post-image path when present, else the pre-image
// path. Used as a stable sort key in BlobByRef.
func changePath(c *object.Change) string {
	if c.To.Name != "" {
		return c.To.Name
	}
	return c.From.Name
}
