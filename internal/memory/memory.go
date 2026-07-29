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

// DirReader serves files from a directory.
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

func (r *DirReader) MkdirAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return err
	}
	abs := filepath.Join(r.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) WriteFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	abs := filepath.Join(r.root, filepath.FromSlash(relPath))
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	tmp, err := os.CreateTemp(parent, ".harness-*")
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	return nil
}

func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	abs := filepath.Join(r.root, filepath.FromSlash(relPath))
	if abs == r.root {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	return nil
}

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
	if startDir != root {
		_ = startDir.Close()
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
