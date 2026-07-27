package memory

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
	gogit "github.com/go-git/go-git/v6"
)

// gitDirName is the repository metadata directory, which a working-tree copy
// must never carry across: the destination gets its own.
const gitDirName = ".git"

// EnsureProjectRepo initializes a project memory repo and fills in
// any missing scaffold entries. Existing git repos are opened as-is; missing
// or non-git directories are initialized through go-git.
type ProjectRepoManager struct{}

func (ProjectRepoManager) EnsureProjectRepo(root string, global bool) error {
	return EnsureProjectRepo(root, global)
}

func (ProjectRepoManager) MoveProjectRepo(src, dst string, global bool) error {
	return MoveProjectRepo(src, dst, global)
}

func (ProjectRepoManager) SameProjectRepoPath(a, b string) (bool, error) {
	return SameProjectRepoPath(a, b)
}

// EnsureProjectRepo initializes a project memory repo and fills in
// any missing scaffold entries. Existing git repos are opened as-is; missing
// or non-git directories are initialized through go-git.
func EnsureProjectRepo(root string, global bool) error {
	repo, err := gitw.Init(root)
	if err != nil {
		return err
	}
	if err := CreateMissingProjectRepo(root, global); err != nil {
		return err
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), ProjectRepoScaffoldFiles(global)); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
		slog.Warn("project memory repo scaffold commit", "repo", root, "err", err)
	}
	return nil
}

// MoveProjectRepo copies one project memory repo to another path, excluding the
// source .git directory, then initializes and commits the destination layout.
//
// Source and destination are compared by physical identity before anything is
// copied, and an identity that cannot be established aborts the move. Both
// matter because the copy is destructive at the destination: copyFile opens
// every target with O_TRUNC, so a source and destination that name one
// repository through different spellings — a junction, a symlink, an 8.3
// alias, or a different case on Windows — would walk the tree truncating each
// file to zero and then copying it onto itself. A lexical comparison sees two
// different strings there and proceeds.
func MoveProjectRepo(src, dst string, global bool) error {
	same, err := SameProjectRepoPath(src, dst)
	if err != nil {
		return fmt.Errorf("memory: identify project memory repo: %w", err)
	}
	if same {
		return EnsureProjectRepo(dst, global)
	}
	if err := copyTreeWithoutGit(src, dst); err != nil {
		return err
	}
	if err := EnsureProjectRepo(dst, global); err != nil {
		return err
	}
	repo, err := gitw.Open(dst)
	if err != nil {
		return err
	}
	files, err := listRepoFiles(dst)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "migration"}, "move project memory repo"), files); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
		slog.Warn("project memory repo move commit", "repo", dst, "err", err)
	}
	return nil
}

// copyTreeWithoutGit copies the working tree at src into dst, excluding the
// source .git directory.
//
// The copy refuses every way the destination can turn out to be, or to sit
// inside, the source. They are separate checks because each catches something
// the others cannot:
//
//   - Disjointness by name, before anything is created. Neither tree may be the
//     other or contain it. This runs first only so the ordinary mistake is
//     refused without touching the filesystem; on its own it proves nothing,
//     because it is a fact about names and the handles are opened afterwards.
//   - Disjointness again, after both ends are pinned, against identities that
//     have each been confirmed to describe the directory actually held open.
//     This is the one that counts. Between the first check and MkdirAll the
//     destination's name can be re-pointed into the source — the early check
//     passed, MkdirAll creates the entry inside the source, and the two handles
//     are still different directories, so no identity comparison objects.
//   - Directory identity, from the two pinned handles: the destination *is* the
//     source, reached by another name.
//   - Entry identity, per directory, during traversal, against a subdirectory
//     that has been pinned and is then descended into through that same handle.
//     A one-time proof says where the destination was; this says what is in hand.
//   - File identity, per file. Distinct directories can hold hard links to one
//     inode, which no directory-level check can see.
//
// Pinning both ends also confines the copy: a link inside either tree that
// leaves it is refused rather than read through or written through.
func copyTreeWithoutGit(src, dst string) error {
	return copyTreeWithoutGitHooked(src, dst, nil)
}

// copyTreeWithoutGitHooked is copyTreeWithoutGit with a hook that runs between
// the name-based containment check and the creation of the destination, so a
// test can stage a re-point in exactly that window. Nil on every production
// path.
func copyTreeWithoutGitHooked(src, dst string, afterCheck func()) error {
	if err := refuseByName(src, dst); err != nil {
		return err
	}
	if afterCheck != nil {
		afterCheck()
	}

	srcRoot, srcID, err := rootfs.OpenIdentified(src)
	if err != nil {
		return fmt.Errorf("memory: open source repo %s: %w", src, err)
	}
	defer srcRoot.Close() //nolint:errcheck // read-only handle
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("memory: create destination %s: %w", dst, err)
	}
	dstRoot, dstID, err := rootfs.OpenIdentified(dst)
	if err != nil {
		return fmt.Errorf("memory: open destination repo %s: %w", dst, err)
	}
	defer dstRoot.Close() //nolint:errcheck // closed after the copy

	// A destination created inside the source during the window above is left
	// where it is. Removing it means removing a name, and the name may no longer
	// be the empty directory that was just created — the same reason
	// CreateExclusive leaves its partial file behind.
	if err := refuseContainment(srcID, dstID, src, dst); err != nil {
		return err
	}
	same, err := srcRoot.SameDir(dstRoot)
	if err != nil {
		return fmt.Errorf("memory: compare source and destination repo: %w", err)
	}
	if same {
		return fmt.Errorf("memory: refusing to copy project memory repo %s onto itself, reached as %s", src, dst)
	}
	return (&repoCopy{dstTop: dstRoot}).copyDir(srcRoot, dstRoot, ".", 0)
}

// maxCopyDepth bounds the traversal. A project memory repo is a handful of
// levels deep, so this is unreachable in normal use.
//
// It exists because the failure mode it guards is not a wrong answer but a
// non-answer: with the destination-entry check removed, the traversal descends
// forever, creating directories as it goes. That is worse than an error in
// every way — nothing to read, nothing to report, and a filesystem filling up.
// A bound turns any arrangement nobody anticipated into a diagnosable failure.
const maxCopyDepth = 64

// refuseByName resolves both sides and applies refuseContainment to the result.
// It is the cheap early pass; its answer is about names, and is superseded by
// the same test against handle-bound identities once both ends are pinned.
func refuseByName(src, dst string) error {
	srcID, err := pathid.Resolve(src)
	if err != nil {
		return fmt.Errorf("memory: identify source repo %s: %w", src, err)
	}
	dstID, err := pathid.Resolve(dst)
	if err != nil {
		return fmt.Errorf("memory: identify destination repo %s: %w", dst, err)
	}
	return refuseContainment(srcID, dstID, src, dst)
}

// refuseContainment requires the two trees to be disjoint: neither the same
// directory, nor one inside the other.
//
// An earlier version checked only for a destination inside the source, on the
// reasoning that the reverse cannot recurse and that a file landing on a file is
// caught per-file. Both halves of that were wrong. Take src = /repos/inner and
// dst = /repos: the walk reaches src/inner and writes it to dst/inner, which is
// src — so source files are overwritten with the contents of their own
// subdirectory. The per-file comparison does not object, because it refuses only
// a file being copied onto *itself*, and these are different files.
//
// Requiring disjoint trees is also a property that can be stated and tested,
// where "this direction is safe because…" needs a fresh case analysis every time
// the traversal changes.
func refuseContainment(srcID, dstID pathid.ID, src, dst string) error {
	if srcID.Equal(dstID) {
		return fmt.Errorf("memory: refusing to copy project memory repo %s onto itself, reached as %s", src, dst)
	}
	if srcID.Contains(dstID) {
		return fmt.Errorf("memory: refusing to copy project memory repo %s into itself at %s", src, dst)
	}
	if dstID.Contains(srcID) {
		return fmt.Errorf("memory: refusing to copy project memory repo %s into %s, which contains it", src, dst)
	}
	return nil
}

// repoCopy carries the state a directory walk needs beyond the two handles for
// the level it is on.
type repoCopy struct {
	// dstTop is the pinned destination root. Every source subdirectory is
	// compared against it before the walk descends, so a destination moved into
	// the source mid-copy is caught when it is met rather than by a proof taken
	// before the walk began.
	dstTop *rootfs.Root
	// afterChildCheck runs after a source subdirectory has been pinned and
	// cleared and before the walk descends into it, so a test can stage a swap
	// in that window. Nil on every production path.
	afterChildCheck func(name string)
}

// copyDir copies the contents of the pinned srcDir into the pinned dstDir,
// recursing into subdirectories and skipping the source .git. rel names the
// level for diagnostics only; every operation addresses a single component
// through the handles for this level, never a path from the top.
//
// Each subdirectory is pinned before it is judged, and the walk descends
// through that same handle. Checking a name and then re-resolving it to recurse
// would be two resolutions of one name, and the window between them is enough
// to rename the checked directory aside and move another into its place: the
// check clears the original and the descent enters the impostor, which when it
// is the destination means copying it into itself. Holding the handle removes
// the second resolution, so what was cleared is what is entered.
//
// Holding it is not the same as locking it. Windows does allow this directory
// to be renamed while the walk has it open; the handle keeps following it, which
// is the point — the walk stays with the directory, not with the name.
func (c *repoCopy) copyDir(srcDir, dstDir *rootfs.Root, rel string, depth int) error {
	if depth > maxCopyDepth {
		return fmt.Errorf("memory: project memory repo nests deeper than %d levels at %s — refusing to continue", maxCopyDepth, rel)
	}
	entries, err := srcDir.ReadDir(".")
	if err != nil {
		return fmt.Errorf("memory: read %s in source repo: %w", rel, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == gitDirName && entry.IsDir() {
			continue
		}
		if !entry.IsDir() {
			if err := copyFileBetweenRoots(srcDir, dstDir, name, filepath.Join(rel, name)); err != nil {
				return err
			}
			continue
		}
		if err := c.copyChildDir(srcDir, dstDir, name, filepath.Join(rel, name), depth); err != nil {
			return err
		}
	}
	return nil
}

// copyChildDir pins one source subdirectory, refuses it if it is the
// destination, and descends into it through the handle it just pinned.
func (c *repoCopy) copyChildDir(srcDir, dstDir *rootfs.Root, name, rel string, depth int) error {
	srcChild, err := srcDir.OpenChild(name)
	if err != nil {
		return fmt.Errorf("memory: open %s in source repo: %w", rel, err)
	}
	defer srcChild.Close() //nolint:errcheck // read-only handle

	isDestination, err := srcChild.SameDir(c.dstTop)
	if err != nil {
		return fmt.Errorf("memory: compare %s against the destination repo: %w", rel, err)
	}
	if isDestination {
		return fmt.Errorf("memory: refusing to copy project memory repo into its own destination at %s", rel)
	}
	if c.afterChildCheck != nil {
		c.afterChildCheck(rel)
	}

	if err := dstDir.MkdirAll(name, 0o755); err != nil {
		return fmt.Errorf("memory: create %s in destination repo: %w", rel, err)
	}
	dstChild, err := dstDir.OpenChild(name)
	if err != nil {
		return fmt.Errorf("memory: open %s in destination repo: %w", rel, err)
	}
	defer dstChild.Close() //nolint:errcheck // closed after this level
	return c.copyDir(srcChild, dstChild, rel, depth+1)
}

// copyFileBetweenRoots streams one file from the source repo to the same
// relative path in the destination repo.
//
// The destination is opened without O_TRUNC and compared against the source as
// a filesystem object before a single byte is discarded. Two directories proven
// distinct can still hold hard links to one inode — SameDir answers a question
// about directories, and says nothing about the files inside them. Opening with
// O_TRUNC would empty the source file and only then start reading it, so the
// copy would report success having written a repository full of nothing.
// Truncation is therefore the step after the comparison, never part of the open.
//
// A link that leaves either tree fails the open and aborts the move rather than
// being skipped. Silently dropping a file from an operation the user asked for
// as a move is data loss; refusing is recoverable and says which path is the
// problem.
func copyFileBetweenRoots(srcDir, dstDir *rootfs.Root, name, rel string) error {
	in, err := srcDir.Open(name)
	if err != nil {
		return fmt.Errorf("memory: read %s from source repo: %w", rel, err)
	}
	defer func() { _ = in.Close() }()
	out, err := dstDir.OpenWrite(name, 0o644)
	if err != nil {
		return fmt.Errorf("memory: write %s to destination repo: %w", rel, err)
	}
	defer func() { _ = out.Close() }()

	inInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("memory: describe %s in source repo: %w", rel, err)
	}
	outInfo, err := out.Stat()
	if err != nil {
		return fmt.Errorf("memory: describe %s in destination repo: %w", rel, err)
	}
	if os.SameFile(inInfo, outInfo) {
		return fmt.Errorf("memory: refusing to copy %s onto itself — source and destination are the same file", rel)
	}

	if err := out.Truncate(0); err != nil {
		return fmt.Errorf("memory: truncate %s in destination repo: %w", rel, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("memory: copy %s: %w", rel, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("memory: close %s in destination repo: %w", rel, err)
	}
	return nil
}

func listRepoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == gitDirName && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// SameProjectRepoPath reports whether two project repo paths identify the same
// project memory repository.
//
// Identity is physical, not lexical. The comparison this replaced made two
// paths absolute and compared the strings, so a junction, a symlink, or an 8.3
// alias of one repository compared unequal to the repository itself — and both
// callers treat "different" as permission to overwrite the destination.
//
// It returns an error rather than a bare false when either side cannot be
// resolved: an unlocatable path is not a path known to be somewhere else, and
// the callers here mutate on the strength of the answer.
func SameProjectRepoPath(a, b string) (bool, error) {
	return pathid.Same(a, b)
}
