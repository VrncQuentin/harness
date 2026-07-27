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
// Both ends are pinned before a byte moves, and the refusal below compares the
// two open directories rather than their names. That is what makes the copy
// safe rather than merely checked: MoveProjectRepo's name-based comparison
// happens once, and between it and the first write the destination name can be
// re-pointed at the source — after which every destination file is opened with
// O_TRUNC and the source is emptied one file at a time. A handle cannot be
// re-pointed. Once these two refer to different directories they refer to
// different directories for the rest of the copy, whatever happens to the names
// meanwhile.
//
// Pinning both ends also confines the copy: a link inside either tree that
// leaves it is refused rather than read through or written through.
func copyTreeWithoutGit(src, dst string) error {
	srcRoot, err := rootfs.Open(src)
	if err != nil {
		return fmt.Errorf("memory: open source repo %s: %w", src, err)
	}
	defer srcRoot.Close() //nolint:errcheck // read-only handle
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("memory: create destination %s: %w", dst, err)
	}
	dstRoot, err := rootfs.Open(dst)
	if err != nil {
		return fmt.Errorf("memory: open destination repo %s: %w", dst, err)
	}
	defer dstRoot.Close() //nolint:errcheck // closed after the copy

	same, err := srcRoot.SameDir(dstRoot)
	if err != nil {
		return fmt.Errorf("memory: compare source and destination repo: %w", err)
	}
	if same {
		return fmt.Errorf("memory: refusing to copy project memory repo %s onto itself, reached as %s", src, dst)
	}
	return copyTreeBetweenRoots(srcRoot, dstRoot, ".")
}

// copyTreeBetweenRoots copies the entries of rel from one pinned repo to the
// other, recursing into directories and skipping the source .git.
func copyTreeBetweenRoots(srcRoot, dstRoot *rootfs.Root, rel string) error {
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
			if err := dstRoot.MkdirAll(child, 0o755); err != nil {
				return fmt.Errorf("memory: create %s in destination repo: %w", child, err)
			}
			if err := copyTreeBetweenRoots(srcRoot, dstRoot, child); err != nil {
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
	out, err := dstRoot.Create(rel, 0o644)
	if err != nil {
		return fmt.Errorf("memory: write %s to destination repo: %w", rel, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
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
