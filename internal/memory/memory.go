// Package memory mediates all reads of the on-disk memory repo. M2 ships
// the read-only file-system view used by the prompt assembler; git and
// semantic retrieval land in M3 and M5 without changing this interface.
package memory

import (
	"fmt"
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

// DirLister is an optional capability some Readers expose for
// enumerating subdirectory names. The agent registry uses this when
// available to list agent folders; callers can type-assert on it.
type DirLister interface {
	// ListDirs returns the names of subdirectories directly under
	// relPath, sorted lexicographically. A missing relPath yields an
	// empty slice and no error.
	ListDirs(relPath string) ([]string, error)
}

// DirCreator is an optional capability some Readers expose for
// creating subdirectories under the repo root. The agent registry
// uses this when available to add a new agent folder; callers can
// type-assert on it.
type DirCreator interface {
	// MkdirAll creates relPath and any necessary parents under the
	// repo root. It is a no-op if the directory already exists; if
	// relPath names an existing file, an error is returned.
	MkdirAll(relPath string) error
}

// DirReader serves files from a directory on the local filesystem. It is
// the concrete Reader used in production; tests can use an in-memory fake
// that implements the same interface.
type DirReader struct {
	// Root is the absolute path of the memory repo. It is joined with the
	// relative paths passed to Read/Exists/Glob.
	Root string
}

// Compile-time assertions that *DirReader satisfies Reader and the
// optional DirLister/DirCreator capabilities, per the Uber Go style
// guide's "Verify Interface Compliance" rule. Keeping each on its own
// line surfaces the missing method when one interface drifts.
var (
	_ Reader     = (*DirReader)(nil)
	_ DirLister  = (*DirReader)(nil)
	_ DirCreator = (*DirReader)(nil)
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

// ListDirs implements DirLister.
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

// MkdirAll implements DirCreator.
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
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return fmt.Errorf("memory: absolute path not allowed: %s", rel)
	}
	// Scan segments on both slash flavours so "\\.." is rejected too.
	for _, sep := range []string{"/", "\\"} {
		for _, seg := range strings.Split(rel, sep) {
			if seg == ".." {
				return fmt.Errorf("memory: path escapes repo root: %s", rel)
			}
		}
	}
	return nil
}
