// Package memory mediates reads and writes for the on-disk memory repo.
// It provides the filesystem-backed view used by prompt assembly, agent
// management, sessions, and the memory browser.
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

	"github.com/VrncQuentin/harness/internal/pathid"
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

// Appender is the capability the append-only session log needs: add to the end
// of a file and make the addition durable, without any way to shorten or
// replace what is already there.
type Appender interface {
	// AppendFile appends data to relPath, creating the file and its parent
	// directories when they are missing, and fsyncs before returning. It
	// never truncates.
	AppendFile(relPath string, data []byte) error
}

// SubRooter hands out an independent rooted capability on a directory inside
// the repo, for a component that needs one for its own lifetime.
//
// It returns a handle rather than a path on purpose. A component handed
// "<repo>/index/_episodes" and left to pin it itself pins whatever that name
// resolves to, which need not be inside the repo at all — the name can already
// be a link out of it. Resolving the relative name through the repo's own
// handle is what keeps the answer inside.
type SubRooter interface {
	// SubRoot pins relPath under the repo root, creating it if it does not
	// exist yet. The caller owns and closes the returned root.
	SubRoot(relPath string) (*rootfs.Root, error)
}

// Repo is the full production memory repository surface. Narrower consumers may
// still accept Reader, but runtime wiring and mutable memory features use Repo
// so missing capabilities fail at compile time instead of at request time.
type Repo interface {
	Reader
	FileWriter
	Walker
	Appender
	SubRooter
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

// DirReader serves files from one project memory repo through a directory
// handle pinned for its lifetime. It is the concrete Reader used in production;
// tests can use an in-memory fake that implements the same interface.
//
// The repo root is opened once, by OpenDirReader, and every operation resolves
// its relative path against that open directory. The root's *pathname* is not
// retained, and that is the point rather than an economy: keeping it would
// leave a second way to reach the repo that skips the handle, and the first
// caller in a hurry would use it. What the reader can reach is therefore fixed
// at the moment it was opened — renaming the repo's directory, or replacing the
// name with a link somewhere else, moves the name and not the handle.
//
// It follows that the reader owns an OS resource and must be closed. The
// runtime holds one per active project repo for as long as that project is
// wired up, and closes it when the service graph is torn down or rebuilt. A
// per-operation pin was the alternative and is weaker: it would re-resolve the
// configured root on every read, so every call would be a fresh opportunity for
// the name to mean something else, and multi-step work like the session log or
// the vector index could not hold one directory across its own steps.
type DirReader struct {
	root *rootfs.Root
	// id is the repo's physical identity, resolved once at pin time. It lets
	// a caller confirm a second component that opened the same configured
	// path independently — one that cannot accept a rooted handle — ended up
	// at the same repository, by comparing identities rather than by
	// resolving the path a further time later.
	id pathid.ID
}

// Compile-time assertions for the production memory repo interfaces.
var (
	_ Reader     = (*DirReader)(nil)
	_ Repo       = (*DirReader)(nil)
	_ FileWriter = (*DirReader)(nil)
	_ Walker     = (*DirReader)(nil)
)

// OpenDirReader pins the project memory repo at root and returns a reader that
// performs every operation through that handle. The caller closes it.
//
// The pin is the authorization. Nothing below the returned reader consults the
// pathname again, so an intermediate symlink or Windows junction planted inside
// the repo cannot lead a later read or write out of it — os.Root resolves each
// component against the open directory and refuses one that leaves.
func OpenDirReader(root string) (*DirReader, error) {
	pinned, id, err := rootfs.OpenIdentified(root)
	if err != nil {
		return nil, fmt.Errorf("memory: open repo %s: %w", root, err)
	}
	return &DirReader{root: pinned, id: id}, nil
}

// Close releases the pinned repo handle. The reader is unusable afterwards.
func (r *DirReader) Close() error {
	if err := r.root.Close(); err != nil {
		return fmt.Errorf("memory: close repo: %w", err)
	}
	return nil
}

// Identity returns the physical identity resolved when this reader was
// pinned.
//
// It exists for the one class of caller this reader's own pin cannot cover: a
// second component that has to open the same repository by pathname because
// its own API gives no other way in — go-git, specifically, whose storage is
// addressed by path throughout and cannot be bound to a rootfs handle. Opening
// that second component and this reader from the same configured string is
// not enough on its own to know they ended up at the same place — the two
// opens happen at different moments, and root can have changed between them —
// so the comparison a caller makes with this should be against an identity
// the *other* component resolved at its own open time too, not a fresh
// resolution of the path taken later still: a later resolution only answers
// what the path currently names, which is a different, later question than
// whether the two components opened the same repository.
func (r *DirReader) Identity() pathid.ID { return r.id }

// SubRoot implements SubRooter. relPath is created if it is missing, then
// pinned through the repo handle so the returned root is inside the repo by
// construction rather than by a comparison somebody has to remember to make.
func (r *DirReader) SubRoot(relPath string) (*rootfs.Root, error) {
	if err := checkRel(relPath); err != nil {
		return nil, fmt.Errorf("memory: subroot %s: %w", relPath, err)
	}
	rel := filepath.FromSlash(relPath)
	if err := r.root.MkdirAll(rel, 0o755); err != nil {
		return nil, fmt.Errorf("memory: subroot %s: %w", relPath, err)
	}
	sub, err := r.root.OpenChild(rel)
	if err != nil {
		return nil, fmt.Errorf("memory: subroot %s: %w", relPath, err)
	}
	return sub, nil
}

// Read implements Reader.
func (r *DirReader) Read(relPath string) ([]byte, error) {
	if err := checkRel(relPath); err != nil {
		return nil, err
	}
	b, err := r.root.ReadFile(filepath.FromSlash(relPath))
	if err != nil {
		return nil, fmt.Errorf("memory: read %s: %w", relPath, err)
	}
	return b, nil
}

// ListDirs returns direct subdirectories of relPath.
func (r *DirReader) ListDirs(relPath string) ([]string, error) {
	entries, err := r.readDir(relPath)
	if err != nil {
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
	if err := r.root.MkdirAll(filepath.FromSlash(relPath), 0o755); err != nil {
		return fmt.Errorf("memory: mkdir %s: %w", relPath, err)
	}
	return nil
}

// WriteFile implements FileWriter. It publishes through a temporary file in the
// destination's own directory followed by a rename, both resolved through the
// pinned repo handle, so readers never observe a partial write and the rename
// cannot be redirected outside the repo.
func (r *DirReader) WriteFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	rel := filepath.FromSlash(relPath)
	if parent := filepath.Dir(rel); parent != "." {
		if err := r.root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("memory: write %s: %w", relPath, err)
		}
	}
	if err := r.root.WriteStreamAtomic(rel, bytes.NewReader(data), 0o644); err != nil {
		return fmt.Errorf("memory: write %s: %w", relPath, err)
	}
	return nil
}

// AppendFile implements Appender.
func (r *DirReader) AppendFile(relPath string, data []byte) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	rel := filepath.FromSlash(relPath)
	if parent := filepath.Dir(rel); parent != "." {
		if err := r.root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("memory: append %s: %w", relPath, err)
		}
	}
	if err := r.root.AppendSync(rel, data, 0o644); err != nil {
		return fmt.Errorf("memory: append %s: %w", relPath, err)
	}
	return nil
}

// RemoveAll deletes relPath and everything below it. It refuses a path that
// names the repo root itself so a caller cannot wipe the whole memory repo by
// passing "." or "" past the validator.
func (r *DirReader) RemoveAll(relPath string) error {
	if err := checkRel(relPath); err != nil {
		return fmt.Errorf("memory: remove %s: %w", relPath, err)
	}
	rel := filepath.FromSlash(relPath)
	if cleaned := filepath.Clean(rel); cleaned == "." || cleaned == string(filepath.Separator) {
		return fmt.Errorf("memory: remove %s: refusing to remove repo root", relPath)
	}
	if err := r.root.RemoveAll(rel); err != nil {
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
	entries, err := r.readDir(dir)
	if err != nil {
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
// never sees git plumbing, even when the repo is a real git repo.
//
// The traversal descends through a pinned handle per directory rather than
// re-resolving each subdirectory's name, so a directory swapped in partway
// through cannot make the walk report entries from outside the repo.
func (r *DirReader) Walk(relPath string) ([]Entry, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	prefix := path.Clean(filepath.ToSlash(relPath))
	if prefix == "." || prefix == "/" {
		prefix = ""
	}
	var out []Entry
	err := r.root.Walk(filepath.FromSlash(relPath), func(entry rootfs.WalkEntry) (bool, error) {
		relSlash := filepath.ToSlash(entry.Rel)
		if prefix != "" {
			relSlash = path.Join(prefix, relSlash)
		}
		// Prune .git anywhere under the repo — the memory directory is
		// itself a git repo and we never want plumbing in the UI.
		if entry.Name == gitDirName {
			return true, nil
		}
		out = append(out, Entry{
			Path: relSlash,
			Dir:  entry.Info.IsDir(),
			Size: entry.Info.Size(),
		})
		return false, nil
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

// readDir lists relPath through the pinned repo handle, reporting a missing
// directory as an empty listing so callers tolerate a partially scaffolded
// repo. An empty relPath names the repo root.
func (r *DirReader) readDir(relPath string) ([]fs.DirEntry, error) {
	if relPath != "" {
		if err := checkRel(relPath); err != nil {
			return nil, err
		}
	}
	rel := filepath.FromSlash(relPath)
	if rel == "" {
		rel = "."
	}
	entries, err := r.root.ReadDir(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// checkRel rejects empty, absolute, and traversing paths.
//
// It is a lexical pre-filter and no longer the containment boundary: the pinned
// root is. It stays because it produces a precise error for the ordinary
// mistake — a caller passing an absolute path or a "../" — where the root would
// report a generic resolution failure, and because rejecting those shapes at
// the edge keeps them out of the paths built below. We work on the
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
