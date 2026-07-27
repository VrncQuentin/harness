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
//   - Containment by name, before anything is created. A destination inside the
//     source is not the source, so no identity comparison rejects it — but
//     creating it adds an entry to a tree that is about to be walked. This runs
//     first only so the ordinary mistake is refused without touching the
//     filesystem; on its own it proves nothing, because it is a fact about names
//     and the handles are opened afterwards.
//   - Containment again, after both ends are pinned, against identities that
//     have each been confirmed to describe the directory actually held open.
//     This is the one that counts. Between the first check and MkdirAll the
//     destination's name can be re-pointed into the source — the early check
//     passed, MkdirAll creates the entry inside the source, and the two handles
//     are still different directories, so no identity comparison objects.
//   - Directory identity, from the two pinned handles: the destination *is* the
//     source, reached by another name.
//   - Entry identity, per directory, during traversal. A one-time proof says
//     where the destination was; descending into a directory that is the
//     destination is refused whenever it is met, so moving it into the source
//     mid-copy does not buy anything.
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
	return copyTreeBetweenRoots(srcRoot, dstRoot, ".", 0)
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

// refuseContainment rejects a destination that is the source or sits below it.
//
// The reverse arrangement — a source below the destination — needs no check
// here. It cannot recurse, because creating the destination adds nothing to the
// source tree being walked, and the only harm left is a file landing on a file,
// which the per-file identity comparison refuses.
func refuseContainment(srcID, dstID pathid.ID, src, dst string) error {
	if srcID.Equal(dstID) {
		return fmt.Errorf("memory: refusing to copy project memory repo %s onto itself, reached as %s", src, dst)
	}
	if srcID.Contains(dstID) {
		return fmt.Errorf("memory: refusing to copy project memory repo %s into itself at %s", src, dst)
	}
	return nil
}

// copyTreeBetweenRoots copies the entries of rel from one pinned repo to the
// other, recursing into directories and skipping the source .git.
func copyTreeBetweenRoots(srcRoot, dstRoot *rootfs.Root, rel string, depth int) error {
	if depth > maxCopyDepth {
		return fmt.Errorf("memory: project memory repo nests deeper than %d levels at %s — refusing to continue", maxCopyDepth, rel)
	}
	entries, err := srcRoot.ReadDir(rel)
	if err != nil {
		return fmt.Errorf("memory: read %s in source repo: %w", rel, err)
	}
	for _, entry := range entries {
		if entry.Name() == gitDirName && entry.IsDir() {
			continue
		}
		child := filepath.Join(rel, entry.Name())
		if entry.IsDir() {
			// Never descend into the destination. The containment checks proved
			// where the destination was before the walk started; this asks of
			// the directory actually in hand, so a destination moved into the
			// source while the copy runs is caught by the same test.
			isDestination, err := srcRoot.SameDirAt(child, dstRoot)
			if err != nil {
				return fmt.Errorf("memory: compare %s against the destination repo: %w", child, err)
			}
			if isDestination {
				return fmt.Errorf("memory: refusing to copy project memory repo into its own destination at %s", child)
			}
			if err := dstRoot.MkdirAll(child, 0o755); err != nil {
				return fmt.Errorf("memory: create %s in destination repo: %w", child, err)
			}
			if err := copyTreeBetweenRoots(srcRoot, dstRoot, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := copyFileBetweenRoots(srcRoot, dstRoot, child); err != nil {
			return err
		}
	}
	return nil
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
func copyFileBetweenRoots(srcRoot, dstRoot *rootfs.Root, rel string) error {
	in, err := srcRoot.Open(rel)
	if err != nil {
		return fmt.Errorf("memory: read %s from source repo: %w", rel, err)
	}
	defer func() { _ = in.Close() }()
	out, err := dstRoot.OpenWrite(rel, 0o644)
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
