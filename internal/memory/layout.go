package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"syscall"

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
	pinned, err := openRepoRoot(root)
	if err != nil {
		return nil, err
	}
	defer pinned.Close() //nolint:errcheck // read-only handle
	return missingItems(pinned, ExpectedProjectRepoLayout(global))
}

// missingItems reports which expected entries are absent or of the wrong kind,
// inspecting each through the pinned repo handle.
func missingItems(pinned *rootfs.Root, expected []LayoutItem) ([]LayoutItem, error) {
	var missing []LayoutItem
	for _, item := range expected {
		if err := checkRel(item.Path); err != nil {
			return nil, fmt.Errorf("memory: layout %s: %w", item.Path, err)
		}
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
//
// The repo is pinned once and both checks run through that one handle, so the
// .git directory that is found and the layout that is inspected are known to
// belong to the same repository. Validating by pathname twice would let the
// name mean one repository for the git check and another for the layout check.
func ValidateProjectRepo(root string, global bool) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("memory: project memory repo path is required")
	}
	pinned, err := openRepoRoot(root)
	if err != nil {
		return err
	}
	defer pinned.Close() //nolint:errcheck // read-only handle
	if err := validateGitDir(pinned, root); err != nil {
		return err
	}
	missing, err := missingItems(pinned, ExpectedProjectRepoLayout(global))
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
// reader, and every create resolves through a handle pinned on the repo root,
// so neither a hand-rolled LayoutItem nor a layout directory that is really a
// link somewhere else can put a file outside the repository.
func CreateMissing(root string, items []LayoutItem) error {
	pinned, err := openRepoRoot(root)
	if err != nil {
		return err
	}
	defer pinned.Close() //nolint:errcheck // closed after the scaffold
	return createMissing(pinned, items)
}

// createMissing creates each item through the pinned repo root.
//
// Nothing here can destroy an existing object, which is what lets it decide
// what to do from a preceding Stat without that being a check/use hazard:
// MkdirAll accepts a directory that is already there and refuses anything else,
// and every file is created with O_EXCL, so a name that filled in after the
// Stat is skipped rather than overwritten. The Stat only chooses whether to
// leave a wrong-kind entry alone; it never authorizes a write.
func createMissing(pinned *rootfs.Root, items []LayoutItem) error {
	for _, item := range items {
		if err := checkRel(item.Path); err != nil {
			return fmt.Errorf("memory: scaffold %s: %w", item.Path, err)
		}
		rel := filepath.FromSlash(item.Path)

		// Skip anything already present of the wrong kind so a conflict on
		// disk never gets clobbered by the scaffolder. Existing directories
		// still get a .gitkeep so backups preserve empty layout directories.
		st, err := pinned.Stat(rel)
		switch {
		case err == nil && st.IsDir() != item.Dir:
			continue
		case err == nil && !item.Dir:
			continue
		case err != nil && !isMissingLayoutPath(err):
			return fmt.Errorf("memory: stat %s: %w", item.Path, err)
		}

		if item.Dir {
			if err := pinned.MkdirAll(rel, 0o755); err != nil {
				return fmt.Errorf("memory: create dir %s: %w", item.Path, err)
			}
			if err := createGitkeep(pinned, item.Path); err != nil {
				return err
			}
			continue
		}

		if parent := filepath.Dir(rel); parent != "." {
			if err := pinned.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("memory: create parent of %s: %w", item.Path, err)
			}
		}
		if err := pinned.CreateExclusive(rel, nil, 0o644); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("memory: create file %s: %w", item.Path, err)
		}
	}
	return nil
}

// createGitkeep places a .gitkeep inside a layout directory, addressing it as a
// path relative to the repo root rather than to the directory.
//
// Addressing it from the repo root is what keeps it in the repo. Handing the
// directory's absolute path to a create — which is what this did before —
// writes wherever that path leads, and a layout directory that is really a
// symlink or a junction leads out of the repository.
func createGitkeep(pinned *rootfs.Root, relDir string) error {
	keep := filepath.Join(filepath.FromSlash(relDir), ".gitkeep")
	if err := pinned.CreateExclusive(keep, nil, 0o644); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("memory: create gitkeep for %s: %w", relDir, err)
	}
	return nil
}

// openRepoRoot pins a project memory repo directory for a scaffold or
// validation sequence. Every inspection and every create in that sequence goes
// through the returned handle, so they all describe one directory even if the
// name is re-pointed while the sequence runs.
func openRepoRoot(root string) (*rootfs.Root, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("memory: repo path is empty")
	}
	pinned, err := rootfs.Open(root)
	if err != nil {
		if isNotDirectory(err) {
			return nil, fmt.Errorf("memory: repo path is not a directory: %s", root)
		}
		return nil, fmt.Errorf("memory: open repo root %s: %w", root, err)
	}
	return pinned, nil
}

// validateGitDir checks the repo's .git through the pinned handle. root is
// carried for the message only; it is never resolved again.
func validateGitDir(pinned *rootfs.Root, root string) error {
	info, err := pinned.Stat(gitDirName)
	if err != nil {
		if isMissingLayoutPath(err) {
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

// isNotDirectory reports whether err says the path exists but is not a
// directory, which each platform spells its own way.
func isNotDirectory(err error) bool {
	for _, errno := range notDirectoryErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
