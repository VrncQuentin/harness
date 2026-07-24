package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v6/plumbing/format/diff"
	"github.com/go-git/go-git/v6/plumbing/format/reflog"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	udiff "github.com/go-git/go-git/v6/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// HeadSHA returns the SHA of the current HEAD commit.
// It returns an empty string if the repo has no commits yet.
func (r *Repo) HeadSHA() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	head, err := r.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("git: head %s: %w", r.path, err)
	}
	return head.Hash().String(), nil
}

// WorkspaceStage stages files for the next commit. Paths are relative to the
// repository root (forward slashes). When files is empty all modified and
// untracked files are staged (equivalent to git add -A).
func (r *Repo) WorkspaceStage(files []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: worktree %s: %w", r.path, err)
	}
	if len(files) == 0 {
		if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
			return fmt.Errorf("git: stage all %s: %w", r.path, err)
		}
		return nil
	}
	for _, f := range files {
		if _, err := wt.Add(f); err != nil {
			return fmt.Errorf("git: add %s: %w", f, err)
		}
	}
	return nil
}

// WorkspaceCommit creates a commit from the current index with msg.
// It appends a reflog entry for ergonomic undo via git reset --hard HEAD@{1}.
// The returned newSHA is the new commit's hex SHA; preOpSHA is the HEAD SHA
// before the commit (empty for an initial commit). preOpSHA is the
// authoritative recovery token — the caller should record it before the commit.
func (r *Repo) WorkspaceCommit(msg string) (newSHA, preOpSHA string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if head, herr := r.repo.Head(); herr == nil {
		preOpSHA = head.Hash().String()
	}

	wt, wtErr := r.repo.Worktree()
	if wtErr != nil {
		return "", "", fmt.Errorf("git: worktree %s: %w", r.path, wtErr)
	}
	name, email := r.authorIdentity()
	now := time.Now()
	hash, commitErr := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: name, Email: email, When: now},
	})
	if commitErr != nil {
		return "", "", fmt.Errorf("git: commit %s: %w", r.path, commitErr)
	}
	newSHA = hash.String()

	// Reflog write — ergonomics only; not the authoritative recovery record.
	// Silently skipped if the storer does not implement ReflogStorer (in-memory).
	if rls, ok := r.repo.Storer.(storer.ReflogStorer); ok {
		if head, herr := r.repo.Head(); herr == nil {
			oldHash := plumbing.ZeroHash
			if preOpSHA != "" {
				oldHash = plumbing.NewHash(preOpSHA)
			}
			_ = rls.AppendReflog(head.Name(), &reflog.Entry{
				OldHash:   oldHash,
				NewHash:   hash,
				Committer: reflog.Signature{Name: name, Email: email, When: now},
				Message:   "commit: " + firstLine(msg),
			})
		}
	}

	return newSHA, preOpSHA, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// StatusEntry is one changed path in porcelain-style notation.
type StatusEntry struct {
	// Staging and Worktree are the porcelain status codes (M, A, D, ?, …).
	Staging  byte
	Worktree byte
	Path     string
}

// Status returns the worktree status as porcelain-style entries, sorted by
// path. A nil slice means the worktree is clean.
func (r *Repo) Status() ([]StatusEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("git: worktree %s: %w", r.path, err)
	}
	st, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("git: status %s: %w", r.path, err)
	}
	var entries []StatusEntry
	for path, fs := range st {
		if fs.Staging == gogit.Unmodified && fs.Worktree == gogit.Unmodified {
			continue
		}
		entries = append(entries, StatusEntry{
			Staging:  byte(fs.Staging),
			Worktree: byte(fs.Worktree),
			Path:     path,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// LogEntry is one commit in a log listing.
type LogEntry struct {
	SHA     string
	Author  string
	When    string // ISO8601
	Summary string // first line of the message
}

// Log returns up to n commits reachable from HEAD, newest first.
func (r *Repo) Log(n int) ([]LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	iter, err := r.repo.Log(&gogit.LogOptions{})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil // empty repo: no commits yet
		}
		return nil, fmt.Errorf("git: log %s: %w", r.path, err)
	}
	defer iter.Close()
	var entries []LogEntry
	for len(entries) < n {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("git: log %s: %w", r.path, err)
		}
		summary, _, _ := strings.Cut(c.Message, "\n")
		entries = append(entries, LogEntry{
			SHA:     c.Hash.String(),
			Author:  fmt.Sprintf("%s <%s>", c.Author.Name, c.Author.Email),
			When:    c.Author.When.Format("2006-01-02T15:04:05Z07:00"),
			Summary: strings.TrimSpace(summary),
		})
	}
	return entries, nil
}

// DiffCommits returns the unified diff between two revisions (e.g. commit
// SHAs, "HEAD", "HEAD~1").
func (r *Repo) DiffCommits(ctx context.Context, fromRev, toRev string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from, err := r.commitAtRev(fromRev)
	if err != nil {
		return "", err
	}
	to, err := r.commitAtRev(toRev)
	if err != nil {
		return "", err
	}
	fromTree, err := from.Tree()
	if err != nil {
		return "", fmt.Errorf("git: tree %s: %w", fromRev, err)
	}
	toTree, err := to.Tree()
	if err != nil {
		return "", fmt.Errorf("git: tree %s: %w", toRev, err)
	}
	patch, err := fromTree.PatchContext(ctx, toTree)
	if err != nil {
		return "", fmt.Errorf("git: diff %s..%s: %w", fromRev, toRev, err)
	}
	return patch.String(), nil
}

func (r *Repo) commitAtRev(rev string) (*object.Commit, error) {
	hash, err := r.repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("git: resolve %q: %w", rev, err)
	}
	c, err := r.repo.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("git: commit %q: %w", rev, err)
	}
	return c, nil
}

// DiffWorktree returns the unified diff between HEAD and the working tree
// (uncommitted changes, staged or not), including untracked files as
// additions. go-git has no porcelain worktree diff, so this walks the status
// entries and diffs HEAD blob content against on-disk content.
func (r *Repo) DiffWorktree(ctx context.Context) (string, error) {
	entries, err := r.Status()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var headTree *object.Tree
	if head, err := r.repo.Head(); err == nil {
		if c, err := r.repo.CommitObject(head.Hash()); err == nil {
			headTree, _ = c.Tree()
		}
	}

	var buf bytes.Buffer
	enc := fdiff.NewUnifiedEncoder(&buf, 3)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		before := r.headContent(headTree, entry.Path)
		after, isBinary := worktreeContent(filepath.Join(r.path, filepath.FromSlash(entry.Path)))
		if before == after {
			continue
		}
		fp := newFilePatch(entry.Path, before, after, isBinary)
		if err := enc.Encode(singlePatch{fp: fp}); err != nil {
			return "", fmt.Errorf("git: encode diff %s: %w", entry.Path, err)
		}
	}
	return buf.String(), nil
}

// headContent returns the blob text for path at HEAD, or "" when the path
// does not exist there (new file) or the repo has no commits.
func (r *Repo) headContent(tree *object.Tree, path string) string {
	if tree == nil {
		return ""
	}
	f, err := tree.File(path)
	if err != nil {
		return ""
	}
	content, err := f.Contents()
	if err != nil {
		return ""
	}
	return content
}

func worktreeContent(absPath string) (content string, isBinary bool) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false // deleted from worktree
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return "", true
	}
	return string(data), false
}

// --- minimal diff.Patch implementation over utils/diff chunks ---

type singlePatch struct {
	fp fdiff.FilePatch
}

func (p singlePatch) FilePatches() []fdiff.FilePatch { return []fdiff.FilePatch{p.fp} }
func (p singlePatch) Message() string                { return "" }

type filePatch struct {
	from, to fdiff.File
	chunks   []fdiff.Chunk
	binary   bool
}

func newFilePatch(path, before, after string, isBinary bool) fdiff.FilePatch {
	fp := filePatch{binary: isBinary}
	if before != "" || after == "" { // exists at HEAD unless it is a pure addition
		fp.from = diffFile{path: path}
	}
	if after != "" || isBinary {
		fp.to = diffFile{path: path}
	}
	if !isBinary {
		for _, d := range udiff.Do(before, after) {
			fp.chunks = append(fp.chunks, textChunk{d})
		}
	}
	return fp
}

func (p filePatch) IsBinary() bool               { return p.binary }
func (p filePatch) Files() (from, to fdiff.File) { return p.from, p.to }
func (p filePatch) Chunks() []fdiff.Chunk        { return p.chunks }

type diffFile struct {
	path string
}

func (f diffFile) Hash() plumbing.Hash     { return plumbing.ZeroHash }
func (f diffFile) Mode() filemode.FileMode { return filemode.Regular }
func (f diffFile) Path() string            { return f.path }

type textChunk struct {
	d diffmatchpatch.Diff
}

func (c textChunk) Content() string { return c.d.Text }

func (c textChunk) Type() fdiff.Operation {
	switch c.d.Type {
	case diffmatchpatch.DiffInsert:
		return fdiff.Add
	case diffmatchpatch.DiffDelete:
		return fdiff.Delete
	default:
		return fdiff.Equal
	}
}
