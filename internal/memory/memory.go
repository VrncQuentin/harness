// Package memory mediates reads and writes for the on-disk memory repo.
// It provides the filesystem-backed view used by prompt assembly, agent
// management, sessions, and the memory browser.
package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

// Reader is the minimum surface the prompt assembler needs from the memory
// repo. All paths are forward-slash and relative to the repo root.
type Reader interface {
	Read(relPath string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

// FileWriter is an optional capability some Readers expose for writing
// files under the repo root.
type FileWriter interface {
	WriteFile(relPath string, data []byte) error
}

// Walker is an optional capability some Readers expose for enumerating
// every entry under a path.
type Walker interface {
	Walk(relPath string) ([]Entry, error)
}

// Repo is the full production memory repository surface.
type Repo interface {
	Reader
	FileWriter
	Walker
	ListDirs(relPath string) ([]string, error)
	MkdirAll(relPath string) error
	RemoveAll(relPath string) error
}

// Entry describes one path under the memory repo as returned by Walk.
type Entry struct {
	Path string
	Dir  bool
	Size int64
}

// DirReader serves files from a directory.  Every read operation creates
// a fresh Anchor, verifies identity, and releases it when done.  Durable
// Anchor ownership is deferred to PR 9.
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
	a, err := rootfs.NewAnchor(root)
	if err != nil {
		return nil, fmt.Errorf("memory: open dir reader %s: %w", root, err)
	}
	_ = a.Close()
	return &DirReader{root: root}, nil
}

func (r *DirReader) openAnchor() (*rootfs.Anchor, error) {
	return rootfs.NewAnchor(r.root)
}

// Read implements Reader.
func (r *DirReader) Read(relPath string) ([]byte, error) {
	if err := checkRel(relPath); err != nil {
		return nil, err
	}
	a, err := r.openAnchor()
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
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

// ListDirs returns direct subdirectories of relPath.
func (r *DirReader) ListDirs(relPath string) ([]string, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	a, err := r.openAnchor()
	if err != nil {
		return nil, fmt.Errorf("memory: list dirs %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		return nil, fmt.Errorf("memory: list dirs %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir(filepath.FromSlash(relPath))
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

// MkdirAll creates relPath and any necessary parents.
func (r *DirReader) MkdirAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return err
	}
	a, err := r.openAnchor()
	if err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.FromSlash(relPath), 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	return nil
}

// WriteFile implements FileWriter via temp+rename through the anchor.
func (r *DirReader) WriteFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	a, err := r.openAnchor()
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(relPath)), 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := root.WriteStreamAtomic(filepath.FromSlash(relPath), strings.NewReader(string(data)), 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	return nil
}

// RemoveAll removes relPath.
func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	if relPath == "" || relPath == "." {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	a, err := r.openAnchor()
	if err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(filepath.FromSlash(relPath)); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	return nil
}

// Glob implements Reader.
func (r *DirReader) Glob(pattern string) ([]string, error) {
	if err := checkRel(pattern); err != nil {
		return nil, err
	}
	dir, file := path.Split(pattern)
	a, err := r.openAnchor()
	if err != nil {
		return nil, fmt.Errorf("memory: glob %s: %w", pattern, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		return nil, fmt.Errorf("memory: glob %s: %w", pattern, err)
	}
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir(filepath.FromSlash(dir))
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

// Walk implements Walker using rooted traversal.  Before entering a
// subdirectory, Lstat is called through the parent to check whether the
// entry is a link — links are refused.  OpenChild follows only after
// the link check passes.  Children are closed after their subtrees,
// and the active ancestor stack is used for cycle detection.
func (r *DirReader) Walk(relPath string) ([]Entry, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	a, err := r.openAnchor()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
	}
	defer func() { _ = a.Close() }()
	root, err := a.Open()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
	}
	defer func() { _ = root.Close() }()

	var out []Entry
	var walkDir func(dir *rootfs.Root, prefix string, ancestors []*rootfs.Root) error
	walkDir = func(dir *rootfs.Root, prefix string, ancestors []*rootfs.Root) error {
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
			info, err := e.Info()
			if err != nil {
				return fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			out = append(out, Entry{Path: childRel, Dir: e.IsDir(), Size: info.Size()})
			if !e.IsDir() {
				continue
			}
			// Lstat through the parent to detect links before
			// OpenChild would follow them.
			fi, err := dir.Lstat(name)
			if err != nil {
				return fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 {
				continue
			}
			child, err := dir.OpenChild(name)
			if err != nil {
				return fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			isCycle := false
			for _, anc := range ancestors {
				same, err := anc.SameDir(child)
				if err != nil {
					_ = child.Close()
					return err
				}
				if same {
					isCycle = true
					break
				}
			}
			if isCycle {
				_ = child.Close()
				continue
			}
			if err := walkDir(child, childRel, append(ancestors, child)); err != nil {
				_ = child.Close()
				return err
			}
			_ = child.Close()
		}
		return nil
	}

	startDir := root
	startPrefix := ""
	startAncestors := []*rootfs.Root{root}
	if relPath != "" {
		fi, err := root.Lstat(filepath.FromSlash(relPath))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 {
			return nil, nil
		}
		child, err := root.OpenChild(filepath.FromSlash(relPath))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
		}
		defer func() { _ = child.Close() }()
		startDir = child
		startPrefix = relPath
		startAncestors = append(startAncestors, child)
	}

	if err := walkDir(startDir, startPrefix, startAncestors); err != nil {
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
