package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// LayoutItem describes one entry the canonical memory layout requires.
// Path is forward-slash and relative to the repo root. Dir distinguishes
// directories from files so callers know whether to MkdirAll or create
// an empty file.
type LayoutItem struct {
	Path string
	Dir  bool
	Desc string
}

// ExpectedProjectRepoLayout returns the canonical layout for one project memory repo
// project memory repository. Every project owns its prompt memory files; the
// global project additionally carries the fallback agent-definition library.
func ExpectedProjectRepoLayout(global bool) []LayoutItem {
	items := []LayoutItem{
		{Path: "sessions.jsonl", Dir: false, Desc: "Project session history"},
		{Path: "episodes", Dir: true, Desc: "Session episode files"},
		{Path: "index", Dir: true, Desc: "Semantic search indexes"},
		{Path: "index/_episodes", Dir: true, Desc: "Episode embeddings"},
		{Path: "artifacts", Dir: true, Desc: "Project artifacts"},
	}
	base := []LayoutItem{
		{Path: "rules.md", Dir: false, Desc: "Project rules"},
		{Path: "user.md", Dir: false, Desc: "Facts about the user for this project"},
		{Path: "facts.md", Dir: false, Desc: "Promoted facts for this project"},
	}
	if global {
		base = append(base, LayoutItem{Path: "agents", Dir: true, Desc: "Global agents library"})
	} else {
		base = append(base, LayoutItem{Path: "agents", Dir: true, Desc: "Project agent overrides"})
	}
	return append(base, items...)
}

// MissingProjectRepoItems returns absent project memory repo entries under root.
func MissingProjectRepoItems(root string, global bool) ([]LayoutItem, error) {
	if root == "" {
		return nil, errors.New("memory: repo path is empty")
	}
	if err := validateRootDir(root); err != nil {
		return nil, err
	}
	expected := ExpectedProjectRepoLayout(global)
	var missing []LayoutItem
	for _, item := range expected {
		abs := filepath.Join(root, filepath.FromSlash(item.Path))
		st, err := os.Stat(abs)
		if isMissingLayoutPath(err) {
			missing = append(missing, item)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("memory: stat %s: %w", item.Path, err)
		}
		if st.IsDir() != item.Dir {
			missing = append(missing, item)
		}
	}
	return missing, nil
}

// CreateMissingProjectRepo creates missing project memory repo entries under root.
func CreateMissingProjectRepo(root string, global bool) error {
	return CreateMissing(root, ExpectedProjectRepoLayout(global))
}

// ProjectRepoScaffoldFiles returns every file created by CreateMissingProjectRepo,
// including .gitkeep files for scaffolded directories. Callers use this list
// for the initial scaffold commit so fresh repos can be backed up and recloned.
func ProjectRepoScaffoldFiles(global bool) []string {
	items := ExpectedProjectRepoLayout(global)
	files := make([]string, 0, len(items))
	for _, item := range items {
		if item.Dir {
			files = append(files, path.Join(item.Path, ".gitkeep"))
			continue
		}
		files = append(files, item.Path)
	}
	return files
}

// ValidateProjectRepo verifies a single project memory repo.
func ValidateProjectRepo(root string, global bool) error {
	if root == "" {
		return errors.New("memory: project memory repo path is required")
	}
	if err := validateRootDir(root); err != nil {
		return err
	}
	if err := validateGitDir(root); err != nil {
		return err
	}
	missing, err := MissingProjectRepoItems(root, global)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("memory: project repo layout incomplete: missing %s", layoutPaths(missing))
	}
	return nil
}

// CreateMissing creates each item in items under root. Files are created
// empty. Directories are created with mode 0o755. Items already present
// (including those of the wrong kind) are left untouched - this function
// will not overwrite or delete anything on disk.
//
// All items are validated against the same traversal rules as the
// reader, so a caller passing a hand-rolled LayoutItem cannot escape
// root.
func CreateMissing(root string, items []LayoutItem) error {
	if root == "" {
		return errors.New("memory: repo path is empty")
	}
	if err := validateRootDir(root); err != nil {
		return err
	}

	for _, item := range items {
		if err := checkRel(item.Path); err != nil {
			return fmt.Errorf("memory: scaffold %s: %w", item.Path, err)
		}
		abs := filepath.Join(root, filepath.FromSlash(item.Path))

		// Skip anything already present so a wrong-kind conflict on
		// disk never gets clobbered by the scaffolder. Existing directories
		// still get a .gitkeep so backups preserve empty layout directories.
		if st, err := os.Stat(abs); err == nil {
			if item.Dir && st.IsDir() {
				if err := createGitkeep(abs, item.Path); err != nil {
					return err
				}
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory: stat %s: %w", item.Path, err)
		}

		if item.Dir {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("memory: create dir %s: %w", item.Path, err)
			}
			if err := createGitkeep(abs, item.Path); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fmt.Errorf("memory: create parent of %s: %w", item.Path, err)
		}
		// O_EXCL guards against a TOCTOU race where the file appeared
		// between Stat and OpenFile; we treat that as "already
		// present, skip" rather than an error.
		f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("memory: create file %s: %w", item.Path, err)
		}
		if cerr := f.Close(); cerr != nil {
			return fmt.Errorf("memory: close %s: %w", item.Path, cerr)
		}
	}
	return nil
}

func createGitkeep(absDir, relPath string) error {
	keepPath := filepath.Join(absDir, ".gitkeep")
	f, err := os.OpenFile(keepPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("memory: create gitkeep for %s: %w", relPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("memory: close gitkeep for %s: %w", relPath, err)
	}
	return nil
}

func validateRootDir(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("memory: stat repo root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: repo path is not a directory: %s", root)
	}
	return nil
}

func validateGitDir(root string) error {
	gitPath := filepath.Join(root, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory: repo path is not a git repo: %s (missing .git)", root)
		}
		return fmt.Errorf("memory: stat .git in %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: repo path is not a plain git repo: %s (.git is not a directory)", root)
	}
	return nil
}

func layoutPaths(items []LayoutItem) string {
	paths := make([]string, 0, len(items))
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	return strings.Join(paths, ", ")
}

func isMissingLayoutPath(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
