package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vrnc/harness/internal/project"
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

// ExpectedLayout returns the canonical list of items the memory repo
// should contain. Project-scoped runtime artifacts (sessions.jsonl,
// queue.wal, index vectors.bin/manifest.json) are intentionally excluded -
// each is owned by the subsystem that writes it (session manager, queue,
// embedder) and gets created on first use under projects/global/.
// Per-agent definition files under agents/<n>/ are also excluded; agents
// are created by the user, so the scaffolder only ensures the parent
// directory exists.
//
// Top-level agents/<n>/ holds the global agents library (persona, rules,
// notes only - definition data). Episodes for the system project live
// under projects/global/episodes/<agent>/, not under the agents/ tree.
func ExpectedLayout() []LayoutItem {
	return []LayoutItem{
		{Path: "global", Dir: true, Desc: "Global prompt content"},
		{Path: "global/rules.md", Dir: false, Desc: "Always-on base prompt"},
		{Path: "global/user.md", Dir: false, Desc: "Hand-authored facts about the user"},
		{Path: "global/facts.md", Dir: false, Desc: "Promoted cross-agent facts"},
		{Path: "agents", Dir: true, Desc: "Global agents library (definition only)"},
		{Path: "projects", Dir: true, Desc: "Per-project session/episode/queue/index data"},
		{Path: "projects/global", Dir: true, Desc: "System project (default scope)"},
		{Path: "projects/global/episodes", Dir: true, Desc: "Session episode files for the system project"},
		{Path: "projects/global/index", Dir: true, Desc: "Semantic search indexes for the system project"},
		{Path: "projects/global/index/_episodes", Dir: true, Desc: "Embeddings of the system project's episodes"},
	}
}

// ProjectLayout returns the canonical layout items for a single project
// identified by slug. For the reserved "global" slug it returns the
// system-project scaffold (no rules.md, no sessions.jsonl). For user
// projects it includes rules.md, agents/, sessions.jsonl, episodes/,
// index/, and index/_episodes/. The queue.wal is intentionally omitted
// — it is created lazily by the queue subsystem on first use.
func ProjectLayout(slug string) ([]LayoutItem, error) {
	if err := project.ValidateSlug(slug); err != nil {
		return nil, fmt.Errorf("memory: invalid project slug %q: %w", slug, err)
	}

	if slug == project.GlobalSlug {
		return []LayoutItem{
			{Path: "projects/global", Dir: true, Desc: "System project (default scope)"},
			{Path: "projects/global/episodes", Dir: true, Desc: "Session episode files for the system project"},
			{Path: "projects/global/index", Dir: true, Desc: "Semantic search indexes for the system project"},
			{Path: "projects/global/index/_episodes", Dir: true, Desc: "Embeddings of the system project's episodes"},
		}, nil
	}

	prefix := "projects/" + slug
	return []LayoutItem{
		{Path: prefix, Dir: true, Desc: "User project"},
		{Path: prefix + "/rules.md", Dir: false, Desc: "Project-specific rules"},
		{Path: prefix + "/agents", Dir: true, Desc: "Project agent definitions"},
		{Path: prefix + "/sessions.jsonl", Dir: false, Desc: "Project session history"},
		{Path: prefix + "/episodes", Dir: true, Desc: "Project episode files"},
		{Path: prefix + "/index", Dir: true, Desc: "Project semantic search indexes"},
		{Path: prefix + "/index/_episodes", Dir: true, Desc: "Project episode embeddings"},
	}, nil
}

// MissingItems returns the subset of ExpectedLayout that is absent under
// root. An item present with the wrong kind (file where a directory is
// expected, or vice versa) is reported as missing so the UI surfaces the
// mismatch; CreateMissing leaves such conflicts alone rather than
// destroying user data.
//
// If root is empty, does not exist, or is not a directory, MissingItems
// returns an error so the caller can distinguish "no repo here" from
// "repo present but incomplete" and avoid scaffolding into the wrong
// place.
func MissingItems(root string) ([]LayoutItem, error) {
	if root == "" {
		return nil, errors.New("memory: repo path is empty")
	}
	if err := validateRootDir(root); err != nil {
		return nil, err
	}
	expected := ExpectedLayout()
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
			// Wrong kind on disk - flag as missing so the UI shows it,
			// but CreateMissing will skip it (won't delete user data).
			missing = append(missing, item)
		}
	}
	return missing, nil
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
		// disk never gets clobbered by the scaffolder.
		if _, err := os.Stat(abs); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory: stat %s: %w", item.Path, err)
		}

		if item.Dir {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return fmt.Errorf("memory: create dir %s: %w", item.Path, err)
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

// ValidateRepo verifies that root is a usable memory repo for prompt assembly
// and API serving. Unlike MissingItems, missing canonical layout entries are
// an error here because the API cannot assemble prompts without them.
func ValidateRepo(root string) error {
	if root == "" {
		return errors.New("memory: memory.repo_path is required")
	}
	if err := validateRootDir(root); err != nil {
		return err
	}
	if err := validateGitDir(root); err != nil {
		return err
	}

	missing, err := MissingItems(root)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("memory: repo layout incomplete: missing %s", layoutPaths(missing))
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
