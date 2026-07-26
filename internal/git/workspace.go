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
	"github.com/go-git/go-git/v6/plumbing/format/index"
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

// WorkspaceStageAndCommit stages files and creates a commit from them as a
// single operation. Paths are relative to the repository root (forward
// slashes); when files is empty all modified and untracked files are staged
// (equivalent to git add -A).
//
// Staging and committing are one indivisible step on purpose. Performed as two
// calls they left two ways to corrupt a user's work in progress:
//
//   - A commit that failed after staging succeeded left the staged changes in
//     the index, so a write that reported failure had in fact half-happened.
//   - Two concurrent calls interleaved, and one task's commit swept up the
//     other's staged files.
//
// The index is snapshotted before staging and restored if the commit fails, so
// a failed call leaves the index as it found it.
//
// The returned newSHA is the new commit's hex SHA; preOpSHA is the HEAD SHA
// before the commit (empty for an initial commit). preOpSHA is the
// authoritative recovery token — the caller should record it before the commit,
// and it stays the recovery path whatever happens to the reflog.
//
// Reflog entries are written to both HEAD and the current branch, which is what
// makes git reset --hard HEAD@{1} resolve for a human afterwards. warn is
// non-nil when the commit succeeded but one of those writes did not: the
// operation is done and must not be reported as failed, but the caller should
// pass the warning on so the missing ergonomic undo is visible.
func (r *Repo) WorkspaceStageAndCommit(files []string, msg string) (newSHA, preOpSHA string, warn error, err error) {
	unlock := r.lockRepo()
	defer unlock()

	if head, herr := r.repo.Head(); herr == nil {
		preOpSHA = head.Hash().String()
	}

	wt, wtErr := r.repo.Worktree()
	if wtErr != nil {
		return "", "", nil, fmt.Errorf("git: worktree %s: %w", r.path, wtErr)
	}

	// Snapshot the index for rollback before it is mutated.
	indexBefore, idxErr := r.snapshotIndex()
	if idxErr != nil {
		return "", "", nil, fmt.Errorf("git: commit %s: snapshot index: %w", r.path, idxErr)
	}

	if stageErr := r.stageLocked(wt, files); stageErr != nil {
		return "", "", nil, errors.Join(stageErr, r.restoreIndex(indexBefore))
	}

	name, email := r.authorIdentity()
	now := time.Now()
	hash, commitErr := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: name, Email: email, When: now},
	})
	if commitErr != nil {
		// Undo the staging this call performed: a failed write must not leave
		// the index holding changes the user did not stage.
		return "", "", nil, errors.Join(
			fmt.Errorf("git: commit %s: %w", r.path, commitErr),
			r.restoreIndex(indexBefore),
		)
	}
	newSHA = hash.String()

	// Reflog write — ergonomics only; preOpSHA remains the authoritative record.
	// Reported as a warning rather than an error: the commit is already made,
	// and failing the call would tell the caller a write did not happen when it
	// did.
	oldHash := plumbing.ZeroHash
	if preOpSHA != "" {
		oldHash = plumbing.NewHash(preOpSHA)
	}
	entry := &reflog.Entry{
		OldHash:   oldHash,
		NewHash:   hash,
		Committer: reflog.Signature{Name: name, Email: email, When: now},
		Message:   "commit: " + firstLine(msg),
	}
	// git records a commit in both logs: HEAD moved, and so did the branch it
	// points at. Only the branch log was written before, which is why
	// HEAD@{1} — which reads .git/logs/HEAD — did not resolve after a harness
	// commit despite this method promising it.
	refs := []plumbing.ReferenceName{plumbing.HEAD}
	if head, herr := r.repo.Head(); herr == nil {
		refs = append(refs, head.Name())
	}
	warn = r.appendReflog(entry, refs...)

	return newSHA, preOpSHA, warn, nil
}

// appendReflog writes entry to each of refs, collecting failures rather than
// stopping at the first: a missing entry in one log does not make the others
// less worth writing. Each failure is labelled with the ref it belongs to.
//
// A storer without reflog support is not a failure — the in-memory storer used
// by tests has none, and there is nothing to write.
func (r *Repo) appendReflog(entry *reflog.Entry, refs ...plumbing.ReferenceName) error {
	rls, ok := r.repo.Storer.(storer.ReflogStorer)
	if !ok {
		return nil
	}
	var errs []error
	for _, ref := range refs {
		if err := rls.AppendReflog(ref, entry); err != nil {
			errs = append(errs, fmt.Errorf("reflog %s: %w", ref, err))
		}
	}
	return errors.Join(errs...)
}

// stageLocked stages files into the index. The caller must hold both locks.
func (r *Repo) stageLocked(wt *gogit.Worktree, files []string) error {
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

// snapshotIndex returns a deep copy of the current index for rollback. Entries
// are copied rather than aliased because staging mutates the index in place.
func (r *Repo) snapshotIndex() (*index.Index, error) {
	current, err := r.repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	snapshot := *current
	snapshot.Entries = make([]*index.Entry, len(current.Entries))
	for i, entry := range current.Entries {
		copied := *entry
		snapshot.Entries[i] = &copied
	}
	return &snapshot, nil
}

// restoreIndex puts back a snapshot taken by snapshotIndex. It runs only after
// an operation has already failed, so its own error is reported alongside the
// original rather than replacing it: the caller needs to know both that the
// write failed and that the index was left dirty.
func (r *Repo) restoreIndex(snapshot *index.Index) error {
	if snapshot == nil {
		return nil
	}
	if err := r.repo.Storer.SetIndex(snapshot); err != nil {
		return fmt.Errorf("git: %s: staged changes could not be rolled back: %w", r.path, err)
	}
	return nil
}

// ErrBranchExists is returned by CreateBranch when the named branch is already
// present. Creating never moves an existing branch: SetReference overwrites a
// ref unconditionally, so the old tip would be discarded with no pre-operation
// record of it anywhere — the opposite of the reversible-write contract.
// Callers that mean to move a branch must do so explicitly.
var ErrBranchExists = errors.New("git: branch already exists")

// CreateBranch creates a local branch named name starting at startPoint
// (a branch name, tag, or commit SHA). If startPoint is empty HEAD is used.
// It returns (sha, preOpSHA, warn, err) where sha is the SHA the branch was
// created at, and preOpSHA is the HEAD SHA at call time (for the caller to
// record). The created ref has no pre-operation value by construction — an
// existing branch is refused with ErrBranchExists — so undoing a create is
// deleting the branch, not restoring a tip.
//
// The existence check and the ref write are both inside the repository-wide
// lock. Split across it they would not be a check at all: two handles could
// each find the name free and then set it to different start points, and the
// second write would silently reset the branch the first had just created.
//
// warn is non-nil when the branch was created but its reflog entry could not be
// written; the branch exists either way. The entry goes to the new branch's log
// only — a branch creation does not move HEAD, so git writes nothing to
// logs/HEAD for it.
func (r *Repo) CreateBranch(name, startPoint string) (sha, preOpSHA string, warn error, err error) {
	unlock := r.lockRepo()
	defer unlock()

	refName := plumbing.NewBranchReferenceName(name)
	if vErr := refName.Validate(); vErr != nil {
		return "", "", nil, fmt.Errorf("git: branch %q: %w", name, vErr)
	}

	// Refuse before touching anything: a create must never reset a branch.
	existing, refErr := r.repo.Reference(refName, false)
	switch {
	case refErr == nil:
		return "", "", nil, fmt.Errorf("%w: %s is at %s", ErrBranchExists, name, existing.Hash())
	case !errors.Is(refErr, plumbing.ErrReferenceNotFound):
		return "", "", nil, fmt.Errorf("git: branch %s: read existing ref: %w", name, refErr)
	}

	// Record HEAD SHA for pre-op tracking.
	if head, herr := r.repo.Head(); herr == nil {
		preOpSHA = head.Hash().String()
	}

	// Resolve the starting commit.
	var startHash plumbing.Hash
	startDesc := startPoint
	if startPoint == "" {
		startDesc = "HEAD"
		head, herr := r.repo.Head()
		if herr != nil {
			return "", "", nil, fmt.Errorf("git: branch %s: resolve HEAD: %w", name, herr)
		}
		startHash = head.Hash()
	} else {
		h, herr := r.repo.ResolveRevision(plumbing.Revision(startPoint))
		if herr != nil {
			return "", "", nil, fmt.Errorf("git: branch %s: resolve %q: %w", name, startPoint, herr)
		}
		startHash = *h
	}

	ref := plumbing.NewHashReference(refName, startHash)
	if err := r.repo.Storer.SetReference(ref); err != nil {
		return "", "", nil, fmt.Errorf("git: branch %s: set ref: %w", name, err)
	}

	// Append reflog for the new branch.
	n, e := r.authorIdentity()
	warn = r.appendReflog(&reflog.Entry{
		OldHash:   plumbing.ZeroHash,
		NewHash:   startHash,
		Committer: reflog.Signature{Name: n, Email: e, When: time.Now()},
		Message:   "branch: Created from " + startDesc,
	}, refName)

	return startHash.String(), preOpSHA, warn, nil
}

// Checkout switches to an existing local branch named name.
// It returns (preOpBranch, preOpSHA, err) where preOpBranch is the short
// branch name before checkout and preOpSHA is the HEAD SHA (both empty for
// repos with no commits).
//
// The movement is recorded in the HEAD reflog, matching git: a checkout moves
// HEAD, not the branch, and the branch tips on either side are untouched.
func (r *Repo) Checkout(name string) (preOpBranch, preOpSHA string, warn error, err error) {
	unlock := r.lockRepo()
	defer unlock()

	// Snapshot pre-op state.
	if head, herr := r.repo.Head(); herr == nil {
		preOpSHA = head.Hash().String()
		preOpBranch = head.Name().Short()
	}

	wt, wtErr := r.repo.Worktree()
	if wtErr != nil {
		return "", "", nil, fmt.Errorf("git: worktree %s: %w", r.path, wtErr)
	}
	refName := plumbing.NewBranchReferenceName(name)
	if coErr := wt.Checkout(&gogit.CheckoutOptions{Branch: refName}); coErr != nil {
		return "", "", nil, fmt.Errorf("git: checkout %s: %w", name, coErr)
	}

	// Record the movement in the HEAD reflog, not the target branch's.
	//
	// The entry was previously appended to refs/heads/<name>, which claimed the
	// branch tip had moved from the old HEAD to the new one. It had not: a
	// checkout leaves both branches where they are. That corrupted the target
	// branch's history with a movement it never made, and left the HEAD reflog
	// with no record at all — so git reflog and HEAD@{n}, which read
	// .git/logs/HEAD, could not see the switch.
	if head, herr := r.repo.Head(); herr == nil {
		n, e := r.authorIdentity()
		warn = r.appendReflog(&reflog.Entry{
			OldHash:   plumbing.NewHash(preOpSHA),
			NewHash:   head.Hash(),
			Committer: reflog.Signature{Name: n, Email: e, When: time.Now()},
			Message:   "checkout: moving from " + preOpBranch + " to " + name,
		}, plumbing.HEAD)
	}

	return preOpBranch, preOpSHA, warn, nil
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
		after, isBinary := worktreeContent(r.path, entry.Path)
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

// worktreeContent returns the on-disk content for the repo-relative relPath.
//
// Nothing here dereferences a symlink. A symlink is reported the way git
// stores one — the link target as the blob content — and a regular file whose
// resolved parent lies outside root is skipped. Reading through links would
// let a link committed inside the repo pull file content from anywhere on the
// filesystem into the diff, escaping the tool sandbox that only ever checked
// the repository root.
func worktreeContent(root, relPath string) (content string, isBinary bool) {
	absPath, ok := worktreeSafePath(root, relPath)
	if !ok {
		return "", false
	}
	fi, err := os.Lstat(absPath)
	if err != nil {
		return "", false // deleted from worktree
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, rlErr := os.Readlink(absPath)
		if rlErr != nil {
			return "", false
		}
		return filepath.ToSlash(target), false
	}
	if !fi.Mode().IsRegular() {
		// Directory, device, socket, or Windows junction (reported irregular):
		// no diffable content of its own.
		return "", false
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", false
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return "", true
	}
	return string(data), false
}

// worktreeSafePath joins a repo-relative status path onto root and reports
// whether every directory component below root is a real directory. Any
// intermediate component that is a reparse point — a symlink, or on Windows a
// junction or mount point — is refused, because reading through one would leave
// the repository entirely.
//
// The check rejects rather than resolves deliberately. filepath.EvalSymlinks
// returns a junction path unchanged instead of resolving it, and errors on any
// path below a junction even where os.ReadFile succeeds, so a containment
// comparison built on it silently accepts an out-of-repo read on Windows.
// Lstat's mode bits are the reliable signal.
func worktreeSafePath(root, relPath string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." || clean == string(filepath.Separator) || filepath.IsAbs(clean) {
		return "", false
	}
	current := root
	components := strings.Split(clean, string(filepath.Separator))
	for i, part := range components {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", false // status paths never climb; refuse if one does
		}
		current = filepath.Join(current, part)
		if i == len(components)-1 {
			break // the leaf is classified by the caller
		}
		fi, err := os.Lstat(current)
		if err != nil {
			return "", false
		}
		if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !fi.IsDir() {
			return "", false
		}
	}
	return current, true
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
