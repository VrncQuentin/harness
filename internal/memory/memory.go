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

	"github.com/VrncQuentin/harness/internal/rootfs"
)

type Reader interface {
	Read(relPath string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

type FileWriter interface {
	WriteFile(relPath string, data []byte) error
}

type Walker interface {
	Walk(relPath string) ([]Entry, error)
}

type Repo interface {
	Reader
	FileWriter
	Walker
	ListDirs(relPath string) ([]string, error)
	MkdirAll(relPath string) error
	RemoveAll(relPath string) error
}

type Entry struct {
	Path string
	Dir  bool
	Size int64
}

type DirReader struct {
	root   string
	anchor *rootfs.Anchor
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
	return &DirReader{root: root, anchor: a}, nil
}

func (r *DirReader) Anchor() *rootfs.Anchor { return r.anchor }
func (r *DirReader) Close() error           { return r.anchor.Close() }

func (r *DirReader) SameDirReader(other *DirReader) (bool, error) {
	return r.anchor.SameAnchor(other.anchor)
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
