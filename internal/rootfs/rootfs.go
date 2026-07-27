// Package rootfs performs filesystem operations through a pinned directory
// handle instead of by pathname.
//
// It is the second half of a pair. internal/pathid answers where a path
// physically is, which is what decides whether a request is inside a boundary
// at all. This package answers the question that comes next: having decided,
// act on that place rather than on the name that led to it.
//
// The distinction matters because validating a pathname and then reopening it
// checks one resolution and acts on another — whatever the name meant during
// the check can be replaced before the open. Canonicalizing the opened target
// and comparing it against a pinned root path is no better, because a pathname
// is not an identity: rename the real root aside, move an attacker's directory
// into the name it vacated, and the target opens inside the attacker's
// directory while canonicalizing to a path that sits under the pinned string.
// The comparison agrees with itself and admits the file.
//
// os.Root removes the pathname from the decision. The directory is held open
// and every component is resolved against that handle, so containment becomes
// an ancestry relationship. What os.Root will not do is choose which of
// several roots owns a caller-supplied absolute path — it only accepts names
// relative to itself. Set does that with pathid and hands back a Target that
// operates through the opened root.
//
// os.Root follows symbolic links that stay inside the root; its guarantee is
// that they cannot leave it. It is a containment boundary, not a ban on links.
// A caller that needs a stricter policy — refusing a linked leaf even when its
// target is in-root — has to express that itself.
//
// This package deliberately does not sandbox subprocesses. A pinned handle
// constrains the operations here and nothing a child process does; command
// containment is a separate problem with a separate answer.
package rootfs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// ErrOutsideRoots reports that a path does not lie within any root of a Set.
// It is distinct from a resolution failure: this one means the question was
// answered, and the answer was no.
var ErrOutsideRoots = errors.New("rootfs: path is outside every root")

// Root is an open directory. Every operation names a path relative to it, and
// no operation can reach outside it — including through a symlink, a Windows
// junction, or a replacement of the directory's name after it was opened.
type Root struct {
	root *os.Root
	name string
}

// Open pins the directory at path. The caller closes the result.
func Open(path string) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: open root %s: %w", path, err)
	}
	return &Root{root: r, name: path}, nil
}

// Name returns the path the root was opened as.
func (r *Root) Name() string { return r.name }

// Close releases the directory handle.
func (r *Root) Close() error { return r.root.Close() }

// ReadFile reads the file at rel, which is resolved through the pinned
// directory.
func (r *Root) ReadFile(rel string) ([]byte, error) { return r.root.ReadFile(rel) }

// Lstat describes rel without following it when it is itself a link.
func (r *Root) Lstat(rel string) (fs.FileInfo, error) { return r.root.Lstat(rel) }

// Readlink returns the target rel points at, without resolving it. A link
// whose target lies outside the root is still readable — reading a link is not
// following one, and callers that represent a link by its target (git stores
// one that way) need the value.
func (r *Root) Readlink(rel string) (string, error) { return r.root.Readlink(rel) }

// Set is an ordered list of root directories. It converts a caller-supplied
// path — absolute or relative, in any spelling — into an operation performed
// inside whichever root physically owns it.
type Set []string

// Open resolves path against the set and pins the root that owns it.
//
// The owning root is chosen by physical identity, so a path reaching a root
// through a link, a junction, an 8.3 alias, or a different case on Windows is
// recognised as being inside it, and a path that leaves a root through one of
// those is not. Resolution failure on either side rejects the call: an
// unlocatable path is not a path known to be safe.
//
// The caller closes the returned Target.
func (s Set) Open(path string) (*Target, error) {
	display, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot make %s absolute: %w", path, err)
	}
	target, err := pathid.Resolve(display)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot resolve %s: %w", path, err)
	}
	for _, root := range s {
		if strings.TrimSpace(root) == "" {
			continue
		}
		rootID, err := pathid.Resolve(root)
		if err != nil {
			return nil, fmt.Errorf("rootfs: cannot resolve root %s: %w", root, err)
		}
		if !rootID.Contains(target) {
			continue
		}
		rel, err := filepath.Rel(rootID.Path(), target.Path())
		if err != nil {
			// Unreachable: Contains has already established a relative route.
			return nil, fmt.Errorf("rootfs: %s relative to %s: %w", target, rootID, err)
		}
		// The root is pinned by its physical path rather than by the spelling
		// the user configured, so an alias repointed between the resolution
		// above and this open cannot redirect the root itself.
		opened, err := Open(rootID.Path())
		if err != nil {
			return nil, err
		}
		return &Target{root: opened, rel: rel, display: display}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrOutsideRoots, path)
}

// Target is a path inside a pinned root, ready to be operated on. It carries
// the display spelling the caller used so locators and messages stay in the
// terms the caller asked in, while every actual operation goes through the
// handle.
type Target struct {
	root    *Root
	rel     string
	display string
}

// Display returns the absolute path the caller addressed, for locators, tool
// output, and error messages.
func (t *Target) Display() string { return t.display }

// Close releases the root handle this target was opened against.
func (t *Target) Close() error { return t.root.Close() }

// Read returns the file's contents.
func (t *Target) Read() ([]byte, error) {
	data, err := t.root.ReadFile(t.rel)
	if err != nil {
		return nil, t.pathError(err)
	}
	return data, nil
}

// ReadDir lists the directory's entries.
func (t *Target) ReadDir() ([]os.DirEntry, error) {
	f, err := t.root.root.Open(t.rel)
	if err != nil {
		return nil, t.pathError(err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, t.pathError(err)
	}
	return entries, nil
}

// Stat describes the target, following it when it is a link. A link that
// leaves the root is refused by the root, not followed.
func (t *Target) Stat() (fs.FileInfo, error) {
	info, err := t.root.root.Stat(t.rel)
	if err != nil {
		return nil, t.pathError(err)
	}
	return info, nil
}

// MkdirAllParent creates the target's parent directories inside the root.
func (t *Target) MkdirAllParent(perm fs.FileMode) error {
	dir := filepath.Dir(t.rel)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	if err := t.root.root.MkdirAll(dir, perm); err != nil {
		return t.pathError(err)
	}
	return nil
}

// WriteAtomic replaces the target's contents through a temporary file in the
// same directory followed by a rename, so a crash never leaves a half-written
// file behind. Both steps resolve through the pinned root, so the rename
// cannot be redirected onto a path outside it.
func (t *Target) WriteAtomic(data []byte, perm fs.FileMode) error {
	tmpRel, f, err := t.root.createTemp(filepath.Dir(t.rel), perm)
	if err != nil {
		return t.pathError(err)
	}
	// The temporary file is removed unless the rename consumes it, so a failed
	// write leaves neither a partial target nor a stray file.
	cleanup := true
	defer func() {
		if cleanup {
			_ = t.root.root.Remove(tmpRel)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return t.pathError(err)
	}
	if err := f.Close(); err != nil {
		return t.pathError(err)
	}
	if err := t.root.root.Rename(tmpRel, t.rel); err != nil {
		return t.pathError(err)
	}
	cleanup = false
	return nil
}

// pathError restates err against the path the caller named. os.Root reports
// the root-relative name, which means nothing to a caller that asked about an
// absolute path — and an error naming "notes.md" when the request was for a
// full path reads like a different file. The wrapped cause is preserved so
// errors.Is(err, fs.ErrNotExist) keeps working.
func (t *Target) pathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &fs.PathError{Op: pe.Op, Path: t.display, Err: pe.Err}
	}
	return err
}

// tempNamePrefix marks a temporary file as an in-flight harness write, so one
// left behind by a crash is recognisable rather than mysterious.
const tempNamePrefix = ".harness-write-"

// createTemp opens a new, uniquely named file in dir, relative to the root.
//
// os.CreateTemp cannot be used here: it takes a pathname, which is exactly
// what the root exists to keep out of the decision. The retry loop plays the
// same role as its own — O_EXCL makes the create fail rather than truncate if
// the name is taken, so a collision costs another attempt and never someone
// else's file.
func (r *Root) createTemp(dir string, perm fs.FileMode) (string, *os.File, error) {
	const attempts = 1000
	for range attempts {
		rel := filepath.Join(dir, tempNamePrefix+rand.Text())
		f, err := r.root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		// The mode is applied to the open handle rather than requested at
		// create time, because a umask can reduce the create permission and
		// would leave the renamed file more restrictive than asked for.
		if err := f.Chmod(perm); err != nil {
			_ = f.Close()
			_ = r.root.Remove(rel)
			return "", nil, err
		}
		return rel, f, nil
	}
	return "", nil, fmt.Errorf("rootfs: no free temporary name in %s after %d attempts", dir, attempts)
}
