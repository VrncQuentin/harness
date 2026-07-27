package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/VrncQuentin/harness/internal/pathid"
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
//
// This pins, validates, and closes — it answers "is root a valid repo right
// now", nothing more. A caller that goes on to open a long-lived reader on the
// same root afterwards would be doing exactly what this function itself
// avoids: validating through one pin and then acting through a second one
// taken later, with a window between them in which root can change and the
// two pins end up describing different repositories. OpenValidatedDirReader
// closes that window by handing back the same pin it validated, for callers
// that need one.
func ValidateProjectRepo(root string, global bool) error {
	pinned, _, err := openValidatedRepoRoot(root, global)
	if err != nil {
		return err
	}
	return pinned.Close()
}

// OpenValidatedDirReader pins root, validates it as a project memory repo
// through that pin, and — only on success — returns a DirReader bound to the
// very same handle rather than a second one opened afterwards by name. The
// caller closes the result.
//
// The DirReader retains the physical identity resolved at pin time, so a
// caller that separately opens the same configured path through an API that
// cannot accept a rooted handle — go-git, notably — can compare that other
// component's own resolved identity against DirReader.Identity() directly,
// rather than re-resolving the path a further time later to make the
// comparison. A fresh resolution at comparison time answers "what does this
// path currently name", which is a different, and later, question than "is
// this the same repository the reader was opened against and the other
// component opened around the same moment".
func OpenValidatedDirReader(root string, global bool) (*DirReader, error) {
	pinned, id, err := openValidatedRepoRoot(root, global)
	if err != nil {
		return nil, err
	}
	return &DirReader{root: pinned, id: id}, nil
}

// openValidatedRepoRoot pins root, identifies it, and validates it, returning
// the still-open pin and its identity on success and closing the pin on
// failure.
func openValidatedRepoRoot(root string, global bool) (*rootfs.Root, pathid.ID, error) {
	if strings.TrimSpace(root) == "" {
		return nil, pathid.ID{}, errors.New("memory: project memory repo path is required")
	}
	pinned, id, err := openIdentifiedRepoRoot(root)
	if err != nil {
		return nil, pathid.ID{}, err
	}
	if err := validateGitDir(pinned, root); err != nil {
		_ = pinned.Close()
		return nil, pathid.ID{}, err
	}
	missing, err := missingItems(pinned, ExpectedProjectRepoLayout(global))
	if err != nil {
		_ = pinned.Close()
		return nil, pathid.ID{}, err
	}
	if len(missing) > 0 {
		_ = pinned.Close()
		return nil, pathid.ID{}, fmt.Errorf("memory: project repo layout incomplete: missing %s", layoutPaths(missing))
	}
	return pinned, id, nil
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
	pinned, _, err := openIdentifiedRepoRoot(root)
	return pinned, err
}

// openIdentifiedRepoRoot is openRepoRoot plus the physical identity resolved
// at pin time, for a caller that will retain the pin and needs to compare it
// against another component's identity later without a further resolution.
func openIdentifiedRepoRoot(root string) (*rootfs.Root, pathid.ID, error) {
	if strings.TrimSpace(root) == "" {
		return nil, pathid.ID{}, errors.New("memory: repo path is empty")
	}
	pinned, id, err := rootfs.OpenIdentified(root)
	if err != nil {
		if rootfs.IsNotDirectory(err) {
			return nil, pathid.ID{}, fmt.Errorf("memory: repo path is not a directory: %s", root)
		}
		return nil, pathid.ID{}, fmt.Errorf("memory: open repo root %s: %w", root, err)
	}
	return pinned, id, nil
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
