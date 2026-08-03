package rootfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// OpenChild pins the directory at rel inside r and returns it as a Root in its
// own right. The caller closes it.
//
// A traversal that means to inspect a subdirectory and then descend into it has
// to hold it, not name it twice. Statting rel and then re-resolving the same rel
// to recurse is two resolutions of one name: between them the directory can be
// renamed aside and another moved into its place, so what was inspected and what
// is entered are different directories. Pinning it once and both inspecting and
// descending through that handle removes the second resolution entirely.
func (r *Root) OpenChild(rel string) (*Root, error) {
	child, err := r.root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	return &Root{root: child}, nil
}

// OpenChildNoFollow opens the single-component name inside r, verifies
// the entry is not a link by Lstat'ing it through the parent, and
// confirms the opened child refers to the same filesystem object as that
// entry via os.SameFile.  Returns the pinned child.  The caller closes it.
//
// name must be a single path component (no separators).  For multi-component
// paths, open each component separately so intermediate symlinks are refused.
//
// This closes the check/use window between Lstat and OpenChild: the
// opened handle is compared against the parent entry, so a swap between
// inspection and entry is detected.
func (r *Root) OpenChildNoFollow(name string) (*Root, fs.FileInfo, error) {
	return r.openChildNoFollow(name, nil)
}

// openChildNoFollow is OpenChildNoFollow with an optional hook that runs
// after OpenRoot and before Lstat.  Tests use it to stage substitutions.
func (r *Root) openChildNoFollow(name string, afterOpen func()) (*Root, fs.FileInfo, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, string(filepath.Separator)) || strings.Contains(name, "/") {
		return nil, nil, fmt.Errorf("rootfs: OpenChildNoFollow requires a single component, got %q", name)
	}
	child, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	if afterOpen != nil {
		afterOpen()
	}
	entryFi, err := r.root.Lstat(name)
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	if entryFi.Mode()&os.ModeSymlink != 0 || entryFi.Mode()&os.ModeIrregular != 0 {
		_ = child.Close()
		return nil, nil, fmt.Errorf("rootfs: refusing to enter link %s", name)
	}
	childFi, err := child.Stat(".")
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	if !os.SameFile(entryFi, childFi) {
		_ = child.Close()
		return nil, nil, fmt.Errorf("rootfs: %s changed between open and verify", name)
	}
	return &Root{root: child}, childFi, nil
}
