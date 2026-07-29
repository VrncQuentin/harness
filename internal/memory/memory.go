// Package memory mediates reads and writes for the on-disk memory repo.
// It provides the filesystem-backed view used by prompt assembly, agent
// management, sessions, and the memory browser.
package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

// Reader is the minimum surface the prompt assembler needs from the memory
// repo. All paths are forward-slash and relative to the repo root.
type Reader interface {
	// Read returns the bytes of relPath. A missing file returns an error
	// that satisfies errors.Is(err, fs.ErrNotExist).
	Read(relPath string) ([]byte, error)

	// Glob returns relative paths of files matching pattern
	// (path.Match syntax), sorted lexicographically. Directories are
	// not included. A missing parent directory yields an empty slice
	// and no error.
	Glob(pattern string) ([]string, error)
}

// FileWriter is an optional capability some Readers expose for
// writing files under the repo root. The agent registry and the UI
// memory editor both use this to persist edits to markdown files;
// callers can type-assert on it.
type FileWriter interface {
	// WriteFile writes data to relPath, replacing any existing file
	// at that path. The parent directory is created if missing.
	// Implementations must publish the new content atomically so
	// concurrent readers never observe a partial write.
	WriteFile(relPath string, data []byte) error
}

// Walker is an optional capability some Readers expose for enumerating
// every entry under a path. The UI memory page uses it to render the
// repo as a tree with token estimates per file.
type Walker interface {
	// Walk returns all entries under relPath (excluding relPath
	// itself), depth-first, sorted lexicographically. An empty
	// relPath enumerates the whole repo. A missing relPath yields
	// an empty slice and no error so callers can tolerate a
	// partially-scaffolded repo. The .git directory is skipped so
	// internal git plumbing never leaks into the UI.
	Walk(relPath string) ([]Entry, error)
}

// Repo is the full production memory repository surface. Narrower consumers may
// still accept Reader, but runtime wiring and mutable memory features use Repo
// so missing capabilities fail at compile time instead of at request time.
type Repo interface {
	Reader
	FileWriter
	Walker
	ListDirs(relPath string) ([]string, error)
	MkdirAll(relPath string) error
	RemoveAll(relPath string) error
}

// Entry describes one path under the memory repo as returned by Walk.
// Path is forward-slash relative to the repo root.
type Entry struct {
	Path string
	Dir  bool
	Size int64
}

// DirReader serves files from a directory.  Read operations use a
// pinned os.Root handle for containment.  Durable identity across
// operations is deferred to PR 2c.
type DirReader struct {
	root string
}

var (
	_ Reader     = (*DirReader)(nil)
	_ Repo       = (*DirReader)(nil)
	_ FileWriter = (*DirReader)(nil)
	_ Walker     = (*DirReader)(nil)
)

func NewDirReader(root string) (*DirReader, error) {
	// Validate the directory exists by opening it once.
	r, err := rootfs.Open(root)
	if err != nil {
		return nil, fmt.Errorf("memory: open dir reader %s: %w", root, err)
	}
	_ = r.Close()
	return &DirReader{root: root}, nil
}

// openRoot opens the configured root.  The caller closes it.  Durable
// identity across operations is deferred to PR 2c.
func (r *DirReader) openRoot() (*rootfs.Root, error) {
	return rootfs.Open(r.root)
}

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
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	// Ensure parent directory exists before WriteStreamAtomic.
	if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(relPath)), 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := root.WriteStreamAtomic(filepath.FromSlash(relPath), strings.NewReader(string(data)), 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	if relPath == "" || relPath == "." {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	root, err := r.openRoot()
	if err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(filepath.FromSlash(relPath)); err != nil {
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

// Walk implements Walker using rooted traversal.  Subdirectories are
// entered through OpenChildNoFollow, which opens, Lstats through the
// parent, rejects links, and verifies the opened handle via os.SameFile.
// Metadata comes from the verified child handle.  Children are closed
// after their subtrees.  Links are refused at every component, so
// ordinary directory trees cannot cycle; bind mounts remain out of scope.
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

	startDir := root
	defer func() {
		if startDir != root {
			_ = startDir.Close()
		}
	}()
	startPrefix := ""
	if relPath != "" {
		components := strings.Split(filepath.FromSlash(relPath), string(filepath.Separator))
		for _, comp := range components {
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

	if err := walkDir(startDir, startPrefix); err != nil {
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
