package memory

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gogit "github.com/go-git/go-git/v6"
	gitw "github.com/vrnc/harness/internal/git"
)

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

func (ProjectRepoManager) SameProjectRepoPath(a, b string) bool {
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
func MoveProjectRepo(src, dst string, global bool) error {
	if SameProjectRepoPath(src, dst) {
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

func copyTreeWithoutGit(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}
	return filepath.WalkDir(src, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, srcPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		return copyFile(srcPath, dstPath)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

func listRepoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
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
// filesystem location using the current OS path case-sensitivity rules.
func SameProjectRepoPath(a, b string) bool {
	ac, aerr := filepath.Abs(filepath.Clean(a))
	bc, berr := filepath.Abs(filepath.Clean(b))
	if aerr != nil || berr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ac, bc)
	}
	return ac == bc
}
