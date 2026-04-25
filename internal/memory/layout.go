package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
// should contain. Runtime artifacts (index/vectors.bin, index/manifest.json,
// runtime/sessions.jsonl, runtime/queue.wal) are intentionally excluded -
// each is owned by the subsystem that writes it (embedder, session
// manager, queue) and gets created on first use. Per-agent files under
// agents/<n>/ are also excluded; agents are created by the user, so the
// scaffolder only ensures the parent directory exists.
func ExpectedLayout() []LayoutItem {
	return []LayoutItem{
		{Path: "global", Dir: true, Desc: "Global prompt content"},
		{Path: "global/rules.md", Dir: false, Desc: "Always-on base prompt"},
		{Path: "global/user.md", Dir: false, Desc: "Hand-authored facts about the user"},
		{Path: "global/facts.md", Dir: false, Desc: "Promoted cross-agent facts"},
		{Path: "agents", Dir: true, Desc: "Agent personas, notes and episodes"},
		{Path: "index", Dir: true, Desc: "Semantic search index"},
		{Path: "runtime", Dir: true, Desc: "Session log and queue WAL"},
	}
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
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("memory: stat repo root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("memory: repo path is not a directory: %s", root)
	}

	expected := ExpectedLayout()
	var missing []LayoutItem
	for _, item := range expected {
		abs := filepath.Join(root, filepath.FromSlash(item.Path))
		st, err := os.Stat(abs)
		if errors.Is(err, fs.ErrNotExist) {
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
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("memory: stat repo root %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("memory: repo path is not a directory: %s", root)
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
