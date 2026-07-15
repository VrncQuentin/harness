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
)

// Reader is the minimum surface the prompt assembler needs from the memory
// repo. All paths are forward-slash and relative to the repo root.
type Reader interface {
	// Read returns the bytes of relPath. A missing file returns an error
	// that satisfies errors.Is(err, fs.ErrNotExist).
	Read(relPath string) ([]byte, error)

	// Exists reports whether relPath refers to an existing file. It
	// returns false for directories, traversal attempts, and stat errors.
	Exists(relPath string) bool

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

// DirReader serves files from a directory on the local filesystem. It is
// the concrete Reader used in production; tests can use an in-memory fake
// that implements the same interface.
type DirReader struct {
	// Root is the absolute path of the memory repo. It is joined with the
	// relative paths passed to Read/Exists/Glob.
	Root string
}

// Compile-time assertions for the production memory repo interfaces.
var (
	_ Reader     = (*DirReader)(nil)
	_ Repo       = (*DirReader)(nil)
	_ FileWriter = (*DirReader)(nil)
	_ Walker     = (*DirReader)(nil)
)

// NewDirReader returns a DirReader rooted at root.
func NewDirReader(root string) *DirReader {
	return &DirReader{Root: root}
}

// Read implements Reader.
func (r *DirReader) Read(relPath string) ([]byte, error) {
	abs, err := r.resolve(relPath)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", relPath, err)
	}
	return b, nil
}

// Exists implements Reader.
func (r *DirReader) Exists(relPath string) bool {
	abs, err := r.resolve(relPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(abs)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// ListDirs returns direct subdirectories of relPath.
func (r *DirReader) ListDirs(relPath string) ([]string, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	abs := filepath.Join(r.Root, filepath.FromSlash(relPath))
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
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
	abs := filepath.Join(r.Root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	return nil
}

// WriteFile implements FileWriter. It writes via a temp file in the
// same directory followed by os.Rename so readers never observe a partial
// write mid-flight.
func (r *DirReader) WriteFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	abs := filepath.Join(r.Root, filepath.FromSlash(relPath))
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}

	tmp, err := os.CreateTemp(parent, ".harness-*")
	if err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	tmpPath := tmp.Name()
	// cleanup runs on every error path before the rename succeeds; once
	// the rename lands the temp file no longer exists under tmpPath.
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

// RemoveAll refuses paths that resolve to
// the repo root itself so a caller cannot wipe the whole memory repo
// by passing "." or "" past the validator.
func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	abs := filepath.Join(r.Root, filepath.FromSlash(relPath))
	if abs == r.Root {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	return nil
}

// Glob implements Reader.
func (r *DirReader) Glob(pattern string) ([]string, error) {
	if err := checkRel(pattern); err != nil {
		return nil, err
	}
	// Match against the pattern's parent directory so callers with no
	// metacharacters (e.g. a literal file path) still work. When the
	// parent does not exist we treat it as an empty set so a bare repo
	// without any agent episodes folder doesn't error out.
	dir, file := path.Split(pattern)
	absDir := filepath.Join(r.Root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(absDir)
	if err != nil {
		if os.IsNotExist(err) {
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

// Walk implements Walker. The .git directory is pruned so the editor
// never sees git plumbing, even when memory/ is a real git repo.
func (r *DirReader) Walk(relPath string) ([]Entry, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	absRoot := filepath.Join(r.Root, filepath.FromSlash(relPath))
	var out []Entry
	err := filepath.WalkDir(absRoot, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if absPath == absRoot {
			return nil
		}
		rel, err := filepath.Rel(r.Root, absPath)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		// Prune .git anywhere under the repo - the memory directory is
		// itself a git repo and we never want plumbing in the UI.
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Entry{
			Path: relSlash,
			Dir:  d.IsDir(),
			Size: info.Size(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("memory: walk %s: %w", relPath, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// resolve turns a forward-slash relative path into an absolute OS path.
// It rejects empty, absolute, and traversing inputs explicitly so no
// caller can read a file outside Root by mistake or malice.
func (r *DirReader) resolve(relPath string) (string, error) {
	if err := checkRel(relPath); err != nil {
		return "", err
	}
	return filepath.Join(r.Root, filepath.FromSlash(relPath)), nil
}

// checkRel rejects empty, absolute, and traversing paths. We work on the
// forward-slash string directly so OS-specific separators on Windows
// don't let "a\\..\\b" slip past path.Clean.
func checkRel(rel string) error {
	if rel == "" {
		return fmt.Errorf("memory: empty path")
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || isWindowsAbs(rel) {
		return fmt.Errorf("memory: absolute path not allowed: %s", rel)
	}
	// Scan segments on both slash flavours so "\\.." is rejected too.
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
