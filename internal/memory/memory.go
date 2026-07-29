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
	abs := filepath.FromSlash(relPath)
	parent := filepath.Dir(abs)
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	if err := root.WriteStreamAtomic(abs, strings.NewReader(string(data)), 0o644); err != nil {
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

const maxWalkDepth = 256

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
	type frame struct {
		root  *rootfs.Root
		rel   string
		depth int
	}
	var stack []frame
	seenAncestors := []*rootfs.Root{root}

	if relPath == "" {
		stack = append(stack, frame{root: root, rel: "", depth: 0})
	} else {
		child, err := root.OpenChild(filepath.FromSlash(relPath))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
		}
		seenAncestors = append(seenAncestors, child)
		stack = append(stack, frame{root: child, rel: relPath, depth: 0})
	}

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := cur.root.ReadDir(".")
		if err != nil {
			return nil, fmt.Errorf("memory: walk %s: %w", cur.rel, err)
		}
		for _, e := range entries {
			name := e.Name()
			if name == ".git" {
				continue
			}
			childRel := path.Join(cur.rel, name)
			if cur.rel == "" {
				childRel = name
			}
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			out = append(out, Entry{Path: childRel, Dir: e.IsDir(), Size: info.Size()})
			if !e.IsDir() {
				continue
			}
			if cur.depth+1 >= maxWalkDepth {
				return nil, fmt.Errorf("memory: walk depth exceeded at %s", childRel)
			}
			child, err := cur.root.OpenChild(name)
			if err != nil {
				return nil, fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			fi, err := child.Lstat(".")
			if err != nil {
				_ = child.Close()
				return nil, fmt.Errorf("memory: walk %s: %w", childRel, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeIrregular != 0 {
				_ = child.Close()
				continue
			}
			isCycle := false
			for _, anc := range seenAncestors {
				same, err := anc.SameDir(child)
				if err != nil {
					_ = child.Close()
					return nil, err
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
			seenAncestors = append(seenAncestors, child)
			stack = append(stack, frame{root: child, rel: childRel, depth: cur.depth + 1})
		}
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
