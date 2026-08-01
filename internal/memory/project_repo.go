package memory

import (
	"errors"
	"fmt"
	"log/slog"
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
	return ensureProjectRepoHooked(root, global, nil)
}

// ensureProjectRepoHooked is EnsureProjectRepo with a hook that runs between
// the git repository being opened (and its boundary pinned) and the scaffold
// handle being opened, so a test can stage a re-point in exactly that window.
// Nil on every production path.
func ensureProjectRepoHooked(root string, global bool, afterOpen func()) error {
	repo, err := gitw.Init(root)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	// Scaffold writes and the follow-up commit run inside one repository-wide
	// mutation transaction: the coordinator is held across both, so no other
	// git mutation or index publication on this repository can interleave
	// between them. The commit goes through the transaction session's commit
	// path, which does not reacquire the non-reentrant gate.
	return repo.WithMutation(func(m *gitw.Mutation) error {
		if afterOpen != nil {
			afterOpen()
		}
		pinned, _, err := rootfs.OpenIdentified(root)
		if err != nil {
			return fmt.Errorf("memory: pin repo root %s: %w", root, err)
		}
		defer func() { _ = pinned.Close() }()
		// The transaction gate belongs to the directory git opened. The
		// scaffold handle is bound to that same physical boundary before any
		// write: if the name was re-pointed between the two opens, writing
		// through this handle would scaffold one repository while holding
		// another repository's coordinator.
		same, err := repo.SameRoot(pinned)
		if err != nil {
			return fmt.Errorf("memory: compare repo boundary: %w", err)
		}
		if !same {
			return fmt.Errorf("memory: repo %s changed since it was opened — refusing to scaffold", root)
		}
		if err := createMissingRooted(pinned, ExpectedProjectRepoLayout(global)); err != nil {
			return err
		}
		if _, err := m.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), ProjectRepoScaffoldFiles(global)); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
			slog.Warn("project memory repo scaffold commit", "repo", root, "err", err)
		}
		return nil
	})
}

// MoveProjectRepo copies one project memory repo to another path, excluding the
// source .git directory, then initializes and commits the destination layout.
//
// Source and destination are compared by physical identity before anything is
// copied, and an identity that cannot be established aborts the move. Identity
// has to be physical because a lexical comparison sees two different strings
// where a junction, a symlink, an 8.3 alias, or a different case on Windows
// names one repository — and then the copy proceeds to rewrite the repository
// with itself.
//
// The destination is opened or initialized first, and the copy, scaffolding,
// file enumeration, and migration commit all run inside one repository-wide
// mutation transaction on the destination. Nothing else can interleave with
// the copy or between the writes and the commit, and the commit uses the
// transaction session's commit path, which does not reacquire the gate.
func MoveProjectRepo(src, dst string, global bool) error {
	return moveProjectRepoHooked(src, dst, global, nil)
}

// moveProjectRepoHooked is MoveProjectRepo with a hook that runs after the
// source and destination roots are both pinned and before the copy reads or
// writes anything, so a test can re-point an alias in exactly that window and
// prove the copy stays bound to the pinned objects. Nil on every production
// path.
func moveProjectRepoHooked(src, dst string, global bool, afterPinned func()) error {
	dst = filepath.Clean(dst)
	same, err := SameProjectRepoPath(src, dst)
	if err != nil {
		return fmt.Errorf("memory: identify project memory repo: %w", err)
	}
	if same {
		return EnsureProjectRepo(dst, global)
	}
	// Name-based containment before anything is created: a destination inside
	// the source must be refused before the destination is opened, or the
	// open would initialize a repository inside the tree about to be copied.
	// The pinned containment check inside the transaction remains the
	// authoritative one for re-points after this point.
	if err := refuseByName(src, dst); err != nil {
		return err
	}
	repo, err := gitw.Init(dst)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	return repo.WithMutation(func(m *gitw.Mutation) error {
		dstRoot, dstID, err := rootfs.OpenIdentified(dst)
		if err != nil {
			return fmt.Errorf("memory: pin destination repo %s: %w", dst, err)
		}
		defer func() { _ = dstRoot.Close() }()
		// The transaction gate belongs to the directory git opened. The copy
		// destination handle is bound to that same physical boundary before
		// anything is written, so a name re-pointed between the two opens
		// fails closed instead of copying into one repository while holding
		// another repository's coordinator.
		sameBoundary, err := repo.SameRoot(dstRoot)
		if err != nil {
			return fmt.Errorf("memory: compare destination repo boundary: %w", err)
		}
		if !sameBoundary {
			return fmt.Errorf("memory: destination repo %s changed since it was opened — refusing to move", dst)
		}
		if err := copyTreeToPinnedRootHooked(src, dstRoot, dstID, dst, afterPinned); err != nil {
			return err
		}
		if err := createMissingRooted(dstRoot, ExpectedProjectRepoLayout(global)); err != nil {
			return err
		}
		files, err := listRepoFilesRooted(dstRoot)
		if err != nil {
			return err
		}
		if len(files) > 0 {
			if _, err := m.Commit(gitw.BuildMessage(map[string]string{"type": "migration"}, "move project memory repo"), files); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
				slog.Warn("project memory repo move commit", "repo", dst, "err", err)
			}
		}
		return nil
	})
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
//   - Level by level during the traversal, against a stack of pinned handles for
//     each tree. A one-time proof says where the two trees were; this says what
//     is in hand at every level, in both directions, which is what catches a
//     directory being moved from one tree into the other while the copy runs.
//
// Files need no comparison at all, because they are published by rename rather
// than by truncating the destination — see copyFileBetweenRoots.
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
	if err := createDestinationDir(dst); err != nil {
		return err
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
	return copyTreeBetweenRoots(srcRoot, srcID, dstRoot, dstID, src, dst)
}

// copyTreeToPinnedRoot copies src into a destination that is already pinned
// and verified. The caller holds the pinned destination (and, when the
// destination is a git repository, its coordinator) and passes its verified
// identity, so the copy does not open the destination name a second time.
func copyTreeToPinnedRoot(src string, dstRoot *rootfs.Root, dstID pathid.ID, dst string) error {
	return copyTreeToPinnedRootHooked(src, dstRoot, dstID, dst, nil)
}

// copyTreeToPinnedRootHooked is copyTreeToPinnedRoot with a hook that runs
// after both the source and destination roots are pinned, so a test can
// re-point an alias in exactly that window and prove the copy stays bound to
// the pinned objects. Nil on every production path.
func copyTreeToPinnedRootHooked(src string, dstRoot *rootfs.Root, dstID pathid.ID, dst string, afterPinned func()) error {
	srcRoot, srcID, err := rootfs.OpenIdentified(src)
	if err != nil {
		return fmt.Errorf("memory: open source repo %s: %w", src, err)
	}
	defer srcRoot.Close() //nolint:errcheck // read-only handle
	if afterPinned != nil {
		afterPinned()
	}
	return copyTreeBetweenRoots(srcRoot, srcID, dstRoot, dstID, src, dst)
}

// copyTreeBetweenRoots refuses the destination if it is, or sits inside, the
// source, then walks the working tree from the two pinned roots.
func copyTreeBetweenRoots(srcRoot *rootfs.Root, srcID pathid.ID, dstRoot *rootfs.Root, dstID pathid.ID, src, dst string) error {
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
	return (&repoCopy{
		srcStack: []*rootfs.Root{srcRoot},
		dstStack: []*rootfs.Root{dstRoot},
	}).copyDir(".")
}

// createDestinationDir creates dst through a pinned handle on its deepest
// existing ancestor, so the create itself cannot be re-pointed by a name
// change between the decision to create it and the MkdirAll. dst may name a
// path with any number of missing ancestor components or a trailing separator;
// both are cleaned and the entire missing tail is created through the pinned
// ancestor.
func createDestinationDir(dst string) error {
	clean := filepath.Clean(dst)
	if clean == "." || clean == string(filepath.Separator) {
		return nil
	}
	ancestor := clean
	for {
		pinned, err := rootfs.Open(ancestor)
		if err != nil {
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return fmt.Errorf("memory: no existing ancestor of destination %s", clean)
			}
			ancestor = parent
			continue
		}
		rel, relErr := filepath.Rel(ancestor, clean)
		if relErr != nil {
			_ = pinned.Close()
			return fmt.Errorf("memory: destination %s relative to %s: %w", clean, ancestor, relErr)
		}
		if rel == "." {
			_ = pinned.Close()
			return nil
		}
		defer pinned.Close() //nolint:errcheck // closed after the create
		if err := pinned.MkdirAll(rel, 0o755); err != nil {
			return fmt.Errorf("memory: create destination %s: %w", clean, err)
		}
		return nil
	}
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

// repoCopy walks the two trees with a stack of pinned handles for each, one per
// level from the top down to the level being copied.
//
// The stacks are what keep the trees disjoint for the whole copy rather than at
// one instant. Comparing a newly pinned source subdirectory against the
// destination root alone is not enough in either direction: the destination can
// be moved into the source, and — on a platform that allows renaming a directory
// somebody holds open — a pinned *source ancestor* can be moved into the name
// the destination child is about to be opened under. The second one is worse,
// because the copy then writes a subdirectory over its own parent, and the
// per-file check passes: those are different files at different relative paths.
//
// So every newly pinned source directory is checked against every pinned
// destination directory, and every newly pinned destination directory against
// every pinned source directory including the one just added.
type repoCopy struct {
	// srcStack and dstStack hold one pinned handle per level, top-level root
	// first. The last entry of each is the level currently being copied.
	srcStack []*rootfs.Root
	dstStack []*rootfs.Root
	// afterChildCheck runs after a source subdirectory has been pinned and
	// cleared and before the destination counterpart is pinned, so a test can
	// stage a swap in that window. Nil on every production path.
	afterChildCheck func(rel string)
}

// anySameDir reports whether candidate is any of the directories in stack.
func anySameDir(candidate *rootfs.Root, stack []*rootfs.Root) (bool, error) {
	for _, pinned := range stack {
		same, err := candidate.SameDir(pinned)
		if err != nil {
			return false, err
		}
		if same {
			return true, nil
		}
	}
	return false, nil
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
func (c *repoCopy) copyDir(rel string) error {
	depth := len(c.srcStack) - 1
	if depth > maxCopyDepth {
		return fmt.Errorf("memory: project memory repo nests deeper than %d levels at %s — refusing to continue", maxCopyDepth, rel)
	}
	srcDir := c.srcStack[depth]
	dstDir := c.dstStack[len(c.dstStack)-1]
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
		if err := c.copyChildDir(name, filepath.Join(rel, name)); err != nil {
			return err
		}
	}
	return nil
}

// copyChildDir pins one source subdirectory and its destination counterpart,
// proves each is disjoint from every level of the other tree already pinned,
// and descends through those two handles.
func (c *repoCopy) copyChildDir(name, rel string) error {
	srcDir := c.srcStack[len(c.srcStack)-1]
	dstDir := c.dstStack[len(c.dstStack)-1]

	srcChild, err := srcDir.OpenChild(name)
	if err != nil {
		return fmt.Errorf("memory: open %s in source repo: %w", rel, err)
	}
	defer srcChild.Close() //nolint:errcheck // read-only handle
	inDestination, err := anySameDir(srcChild, c.dstStack)
	if err != nil {
		return fmt.Errorf("memory: compare %s against the destination repo: %w", rel, err)
	}
	if inDestination {
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

	// srcChild joins the source stack before the destination is judged, so the
	// destination cannot turn out to be the very directory about to be read.
	c.srcStack = append(c.srcStack, srcChild)
	defer func() { c.srcStack = c.srcStack[:len(c.srcStack)-1] }()
	inSource, err := anySameDir(dstChild, c.srcStack)
	if err != nil {
		return fmt.Errorf("memory: compare %s against the source repo: %w", rel, err)
	}
	if inSource {
		return fmt.Errorf("memory: refusing to copy project memory repo onto its own source at %s", rel)
	}

	c.dstStack = append(c.dstStack, dstChild)
	defer func() { c.dstStack = c.dstStack[:len(c.dstStack)-1] }()
	return c.copyDir(rel)
}

// copyFileBetweenRoots streams one file from the source repo to the same name in
// the destination repo, publishing it by rename.
//
// The rename is what protects the source. An earlier version opened the
// destination and truncated it once it had checked the two were not the same
// file — which is not enough, because a destination entry can be a hard link to
// a *different* source file than the one being read. Two different inodes, so
// the comparison passes, and the truncate empties a source file the copy has not
// reached yet. Replacing the entry never writes through a link at all, so no
// comparison is needed and none of the source's links are disturbed.
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
	if err := dstDir.WriteStreamAtomic(name, in, 0o644); err != nil {
		return fmt.Errorf("memory: write %s to destination repo: %w", rel, err)
	}
	return nil
}

// listRepoFilesRooted returns the repo-relative forward-slash commit paths of
// every file in the pinned working tree, excluding .git.
//
// The walk descends each directory through the child handle it inspected, so a
// directory swapped between inspection and entry cannot redirect the traversal
// outside the tree, and a link leaving the tree is refused rather than
// followed.
func listRepoFilesRooted(pinned *rootfs.Root) ([]string, error) {
	entries, err := walkEntriesFrom(pinned, "")
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.Dir {
			files = append(files, entry.Path)
		}
	}
	return files, nil
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
