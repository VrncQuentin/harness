package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/VrncQuentin/harness/internal/coord"
	"github.com/VrncQuentin/harness/internal/rootfs"
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

// ExpectedProjectRepoLayout returns the canonical layout for one project memory
// repo. Every project owns its prompt memory files; the global project
// additionally carries the fallback agent-definition library.
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
	pinned, _, err := rootfs.OpenIdentified(root)
	if err != nil {
		return nil, fmt.Errorf("memory: pin repo root %s: %w", root, err)
	}
	defer func() { _ = pinned.Close() }()
	return missingProjectRepoItemsRooted(pinned, global)
}

// missingProjectRepoItemsRooted inspects a pinned repository tree. The caller
// holds the pinned root; identity and containment come from the handle, not
// from a re-resolution of the configured name.
func missingProjectRepoItemsRooted(pinned *rootfs.Root, global bool) ([]LayoutItem, error) {
	expected := ExpectedProjectRepoLayout(global)
	var missing []LayoutItem
	for _, item := range expected {
		st, err := pinned.Stat(filepath.FromSlash(item.Path))
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
	pinned, _, err := rootfs.OpenIdentified(root)
	if err != nil {
		return fmt.Errorf("memory: pin repo root %s: %w", root, err)
	}
	defer func() { _ = pinned.Close() }()
	if err := validateGitDirRooted(pinned, root); err != nil {
		return err
	}
	missing, err := missingProjectRepoItemsRooted(pinned, global)
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
// The repository tree is pinned and written through the handle, and the
// write holds the repository-wide mutation coordinator for the physical
// repository, so scaffolding an existing repository serializes with git
// commits and index publication on the same object. Callers that already
// hold the coordinator (EnsureProjectRepo inside a git transaction) must use
// createMissingRooted instead of reacquiring the non-reentrant gate.
//
// All items are validated against the same traversal rules as the
// reader, so a caller passing a hand-rolled LayoutItem cannot escape
// root.
func CreateMissing(root string, items []LayoutItem) error {
	return createMissingHooked(root, items, nil)
}

// createMissingHooked is CreateMissing with a hook that runs between the
// pin of the repository tree and the identity resolution that verifies it,
// so a test can stage a re-point in exactly that window. Nil on every
// production path.
func createMissingHooked(root string, items []LayoutItem, afterPin func()) error {
	if root == "" {
		return errors.New("memory: repo path is empty")
	}
	pinned, id, err := rootfs.OpenIdentifiedHooked(root, afterPin)
	if err != nil {
		return fmt.Errorf("memory: pin repo root %s: %w", root, err)
	}
	defer func() { _ = pinned.Close() }()
	gate := coord.For(id)
	gate.Lock()
	defer gate.Unlock()
	return createMissingRooted(pinned, items)
}

// createMissingRooted creates each item through a pinned repository tree.
// The caller holds the coordinator; this function never acquires it, so it
// can run inside a git transaction without reacquiring the non-reentrant
// gate.
func createMissingRooted(pinned *rootfs.Root, items []LayoutItem) error {
	for _, item := range items {
		if err := checkRel(item.Path); err != nil {
			return fmt.Errorf("memory: scaffold %s: %w", item.Path, err)
		}
		rel := filepath.FromSlash(item.Path)

		// Skip anything already present so a wrong-kind conflict on
		// disk never gets clobbered by the scaffolder. Existing directories
		// still get a .gitkeep so backups preserve empty layout directories.
		st, err := pinned.Stat(rel)
		if err == nil {
			if item.Dir && st.IsDir() {
				if err := createGitkeepRooted(pinned, item.Path); err != nil {
					return err
				}
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory: stat %s: %w", item.Path, err)
		}

		if item.Dir {
			if err := pinned.MkdirAll(rel, 0o755); err != nil {
				return fmt.Errorf("memory: create dir %s: %w", item.Path, err)
			}
			if err := createGitkeepRooted(pinned, item.Path); err != nil {
				return err
			}
			continue
		}

		if err := pinned.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return fmt.Errorf("memory: create parent of %s: %w", item.Path, err)
		}
		// O_EXCL guards against a TOCTOU race where the file appeared
		// between Stat and CreateExclusive; we treat that as "already
		// present, skip" rather than an error. The create is exclusive, so
		// an entry that appears concurrently is never overwritten.
		if err := pinned.CreateExclusive(rel, nil, 0o644); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("memory: create file %s: %w", item.Path, err)
		}
	}
	return nil
}

// createGitkeepRooted creates .gitkeep inside the pinned layout directory at
// relPath, exclusively. relPath is repo-relative and validated by the caller.
// A failed exclusive create leaves the partial file in place, matching
// CreateExclusive's contract: removing it would mean removing a name that may
// already belong to someone else.
func createGitkeepRooted(pinned *rootfs.Root, relPath string) error {
	keepRel := filepath.Join(filepath.FromSlash(relPath), ".gitkeep")
	if err := pinned.CreateExclusive(keepRel, nil, 0o644); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("memory: create gitkeep for %s: %w", relPath, err)
	}
	return nil
}

// validateGitDirRooted verifies, through the pinned repository tree, that
// root is a plain git repository with a .git directory. The pinned handle
// confines the check: a .git entry that is a link leaving the root is refused
// rather than followed.
func validateGitDirRooted(pinned *rootfs.Root, root string) error {
	info, err := pinned.Stat(".git")
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
