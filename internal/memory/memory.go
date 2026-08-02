// Package memory mediates reads and writes for the on-disk memory repo.
package memory

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/pathid"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// Reader provides read-only access to the memory repo. Every path is
// resolved relative to the pinned root; absolute paths and ".." escapes
// are rejected.
type Reader interface {
	Read(relPath string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

// FileWriter writes through the pinned root using atomic rename. The
// write lands in a temp file and is renamed over the destination, so a
// crash never leaves a half-written file behind.
type FileWriter interface {
	WriteFile(relPath string, data []byte) error
}

// Appender is the append-only write capability a consumer needs to extend a
// log inside the repo. AppendFile adds to the end of a file, creating the file
// and its missing parent directories when they are absent, and fsyncs before
// returning. It can extend and nothing else: there is no truncate, no replace,
// and no general open, so an append-only audit log is never reachable through
// a spelling that can shorten or rewrite it.
type Appender interface {
	AppendFile(relPath string, data []byte) error
}

// Walker walks the directory tree rooted at relPath, returning every entry
// sorted by path. It skips .git and does not follow symlinks out of the
// pinned root.
type Walker interface {
	Walk(relPath string) ([]Entry, error)
}

// Repo combines reading, writing, walking, and directory management
// through a single pinned root.
type Repo interface {
	Reader
	FileWriter
	Walker
	ListDirs(relPath string) ([]string, error)
	MkdirAll(relPath string) error
	RemoveAll(relPath string) error
}

// Entry describes a single filesystem entry within a Walk result.
type Entry struct {
	Path string
	Dir  bool
	Size int64
}

// DirReader reads and writes files through a pinned directory anchor.
// Every operation resolves paths relative to the anchor, so the caller
// cannot escape the root — not by symlink, junction, or path traversal.
// The anchor's identity is compared against a fresh open on every
// operation, so a directory replaced at the same pathname is refused.
//
// Close releases the pinned handle. After Close, every operation fails.
type DirReader struct {
	anchor   *rootfs.Anchor
	identity pathid.ID
}

var (
	_ Reader     = (*DirReader)(nil)
	_ Repo       = (*DirReader)(nil)
	_ FileWriter = (*DirReader)(nil)
	_ Appender   = (*DirReader)(nil)
	_ Walker     = (*DirReader)(nil)
)

// NewDirReader opens root, pins its identity via an Anchor, and returns a
// DirReader that resolves every operation through that pinned handle. The
// verified physical identity is captured at construction so callers can
// compare readers against the object they actually opened. The caller must
// close the reader when done.
func NewDirReader(root string) (*DirReader, error) {
	pinned, id, err := rootfs.OpenIdentified(root)
	if err != nil {
		return nil, fmt.Errorf("memory: open dir reader %s: %w", root, err)
	}
	return &DirReader{anchor: rootfs.NewAnchorFromRoot(pinned, root), identity: id}, nil
}

func (r *DirReader) Close() error { return r.anchor.Close() }

// Identity returns the verified physical identity of the directory this
// reader pinned and holds open. It is captured at construction and verified
// against the anchor's retained handle, so two readers on one physical
// directory — reached through any spelling — compare Equal, and a repointed
// spelling never silently identifies the replacement.
func (r *DirReader) Identity() pathid.ID { return r.identity }

// SameDirReader reports whether r and other are anchored to the same
// filesystem directory. The comparison uses os.SameFile on the two
// pinned handles — no pathname re-resolution is involved.
//
// This is the handle-level comparison for two readers. Comparing a reader
// against a git repository handle uses SameRepo, which compares this reader's
// retained pinned handle with the repository's retained boundary.
func (r *DirReader) SameDirReader(other *DirReader) (bool, error) {
	return r.anchor.SameAnchor(other.anchor)
}

// SameRepo reports whether this reader and the git repository handle are
// anchored to the same physical directory. Both sides compare their retained
// pinned handles via os.SameFile, so a directory replaced at the same
// pathname between the two opens is detected rather than silently accepted.
func (r *DirReader) SameRepo(repo *gitw.Repo) (bool, error) {
	return repo.SameAnchor(r.anchor)
}

// SubAnchor opens a child directory through the DirReader's pinned handle
// and returns a new Anchor pinned to it. The caller must close it.
func (r *DirReader) SubAnchor(rel string) (*rootfs.Anchor, error) {
	comps := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	var cur *rootfs.Anchor
	for i, comp := range comps {
		parent := r.anchor
		if i > 0 {
			parent = cur
		}
		child, err := parent.OpenChild(comp)
		if err != nil {
			if cur != nil {
				_ = cur.Close()
			}
			return nil, fmt.Errorf("memory: sub anchor %s: %w", rel, err)
		}
		if cur != nil {
			_ = cur.Close()
		}
		cur = child
	}
	return cur, nil
}

func (r *DirReader) openRoot() (*rootfs.Root, error) { return r.anchor.Open() }

func (r *DirReader) Read(relPath string) ([]byte, error) {
	if err := checkRel(relPath); err != nil {
		return nil, err
	}
	root, err := r.openRoot()
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	b, err := root.ReadFile(filepath.FromSlash(relPath))
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", relPath, err)
	}
	return b, nil
}

func (r *DirReader) ListDirs(relPath string) ([]string, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	root, err := r.openRoot()
	if err != nil {
		return nil, fmt.Errorf("memory: list dirs %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	rd := filepath.FromSlash(relPath)
	if rd == "" {
		rd = "."
	}
	entries, err := root.ReadDir(rd)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: list dirs %s: %w", relPath, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *DirReader) MkdirAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return err
	}
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.FromSlash(relPath), 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) WriteFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." {
		return fmt.Errorf("memory: write %s: refusing to write to repo root", relPath)
	}
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := root.WriteStreamAtomic(clean, bytes.NewReader(data), 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	return nil
}

// AppendFile appends data to relPath through the pinned root, creating the
// file and its missing parent directories when they are absent, and fsyncs
// before returning. It never truncates and never replaces what is already
// there: the underlying rootfs.AppendSync opens with O_WRONLY|O_CREATE|O_APPEND
// and nothing more, so an append-only log like sessions.jsonl is never
// reachable through a spelling that can shorten or rewrite it.
//
// Unlike WriteFile's rename publication, AppendFile writes in place, which is
// inherent to appending. A relPath entry that is a hard link to a file
// elsewhere is written through — the same underlying file gains the data. The
// pinned root prevents pathname, symlink, and junction escapes, but it cannot
// distinguish a hard-linked entry from the same file outside the repo; see
// rootfs.AppendSync for the documented limitation.
func (r *DirReader) AppendFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." {
		return fmt.Errorf("memory: append %s: refusing to write to repo root", relPath)
	}
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	if err := root.AppendSync(clean, data, 0o644); err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(clean); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) Glob(pattern string) ([]string, error) {
	if err := checkRel(pattern); err != nil {
		return nil, err
	}
	dir, file := path.Split(pattern)
	root, err := r.openRoot()
	if err != nil {
		return nil, fmt.Errorf("memory: glob %s: %w", pattern, err)
	}
	defer func() { _ = root.Close() }()
	rd := filepath.FromSlash(dir)
	if rd == "" {
		rd = "."
	}
	entries, err := root.ReadDir(rd)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: glob %s: %w", pattern, err)
	}
	matches := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ok, err := path.Match(file, e.Name())
		if err != nil {
			return nil, fmt.Errorf("memory: glob %s: %w", pattern, err)
		}
		if !ok {
			continue
		}
		matches = append(matches, path.Join(strings.TrimSuffix(dir, "/"), e.Name()))
	}
	sort.Strings(matches)
	return matches, nil
}

func (r *DirReader) Walk(relPath string) ([]Entry, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	root, err := r.openRoot()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()

	startDir := root
	defer func() {
		if startDir != root {
			_ = startDir.Close()
		}
	}()
	startPrefix := ""
	if relPath != "" {
		comps := strings.Split(filepath.FromSlash(relPath), string(filepath.Separator))
		for _, comp := range comps {
			child, _, err := startDir.OpenChildNoFollow(comp)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil, nil
				}
				return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
			}
			if startDir != root {
				_ = startDir.Close()
			}
			startDir = child
			if startPrefix == "" {
				startPrefix = comp
			} else {
				startPrefix = path.Join(startPrefix, comp)
			}
		}
	}
	return walkEntriesFrom(startDir, startPrefix)
}

// walkEntriesFrom returns every entry under dir, sorted by path, skipping .git
// and descending each directory through the pinned child handle it inspected.
// prefix names the level for diagnostics and path assembly; "" is the top.
//
// The descent is what keeps the walk inside the tree: a directory swapped
// between inspection and entry cannot redirect the traversal, and a link
// leaving the tree is refused rather than followed.
func walkEntriesFrom(dir *rootfs.Root, prefix string) ([]Entry, error) {
	var out []Entry
	var walkDir func(dir *rootfs.Root, prefix string) error
	walkDir = func(dir *rootfs.Root, prefix string) error {
		entries, err := dir.ReadDir(".")
		if err != nil {
			return fmt.Errorf("memory: walk %s: %w", prefix, err)
		}
		for _, e := range entries {
			name := e.Name()
			if name == ".git" {
				continue
			}
			childRel := path.Join(prefix, name)
			if prefix == "" {
				childRel = name
			}
			if !e.IsDir() {
				info, err := e.Info()
				if err != nil {
					return fmt.Errorf("memory: walk %s: %w", childRel, err)
				}
				out = append(out, Entry{Path: childRel, Dir: false, Size: info.Size()})
				continue
			}
			child, childFi, err := dir.OpenChildNoFollow(name)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			out = append(out, Entry{Path: childRel, Dir: true, Size: childFi.Size()})
			if err := walkDir(child, childRel); err != nil {
				_ = child.Close()
				return err
			}
			_ = child.Close()
		}
		return nil
	}
	if err := walkDir(dir, prefix); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func checkRel(rel string) error {
	if rel == "" {
		return fmt.Errorf("memory: empty path")
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || isWindowsAbs(rel) {
		return fmt.Errorf("memory: absolute path not allowed: %s", rel)
	}
	if hasParentSegment(rel, '/') || hasParentSegment(rel, '\\') {
		return fmt.Errorf("memory: path escapes repo root: %s", rel)
	}
	return nil
}

func isWindowsAbs(rel string) bool {
	if len(rel) < 3 || rel[1] != ':' {
		return false
	}
	if rel[2] != '/' && rel[2] != '\\' {
		return false
	}
	c := rel[0]
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

func hasParentSegment(rel string, sep byte) bool {
	start := 0
	for i := 0; i <= len(rel); i++ {
		if i != len(rel) && rel[i] != sep {
			continue
		}
		if i-start == 2 && rel[start:i] == ".." {
			return true
		}
		start = i + 1
	}
	return false
}
