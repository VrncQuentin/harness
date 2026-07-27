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
// target is in-root — has to express that itself. One link shape is refused
// unconditionally: an absolute target, which is what a Windows junction always
// stores, so a junction is never traversed here even when it points back inside
// the root.
//
// Set sidesteps that for its own callers by addressing the target through the
// physical path pathid resolved rather than the spelling the caller used, so a
// junction on the way in is resolved away before the root ever sees it.
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
	"slices"
	"strings"

	"github.com/VrncQuentin/harness/internal/pathid"
)

// ErrOutsideRoots reports that a path does not lie within any root of a Set.
// It is distinct from a resolution failure: this one means the question was
// answered, and the answer was no.
var ErrOutsideRoots = errors.New("rootfs: path is outside every root")

// Root is an open directory. Every operation names a path relative to it, and
// no operation can reach outside it by pathname — including through a symlink,
// a Windows junction, or a replacement of the directory's name after it was
// opened.
//
// "Outside" means outside the directory tree, not outside the filesystem. On
// Linux os.Root does not stop a path from crossing a bind mount, an ordinary
// mount point, or into /proc; a mount staged inside the root is reachable
// through it. Mount-based escapes are outside this package's threat model —
// staging one needs privileges that already defeat the sandbox — and closing
// them would need openat2 with RESOLVE_NO_XDEV, which has no Windows
// counterpart.
type Root struct {
	root *os.Root
}

// Open pins the directory at path. The caller closes the result.
func Open(path string) (*Root, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: open root %s: %w", path, err)
	}
	return &Root{root: r}, nil
}

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

// Open opens rel for reading. The caller closes it.
func (r *Root) Open(rel string) (*os.File, error) { return r.root.Open(rel) }

// OpenWrite opens rel for writing, creating it if it is absent, and
// deliberately does not truncate. The caller closes it.
//
// Truncation is left to the caller because O_TRUNC destroys the file before the
// caller can look at it, and looking at it is sometimes the whole point. Two
// directories proven distinct can still hold hard links to one inode, so a copy
// that opens its destination with O_TRUNC can empty its own source and only
// then discover they were the same file. Open first, compare the handles with
// os.SameFile, and truncate only once they differ.
func (r *Root) OpenWrite(rel string, perm fs.FileMode) (*os.File, error) {
	return r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE, perm)
}

// MkdirAll creates rel and any missing parents inside the root.
func (r *Root) MkdirAll(rel string, perm fs.FileMode) error {
	return r.root.MkdirAll(rel, perm)
}

// ReadDir lists the entries of rel, sorted by filename. "." is the root itself.
// See Target.ReadDir for why the sort is load-bearing.
func (r *Root) ReadDir(rel string) ([]os.DirEntry, error) {
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// SameDir reports whether r and other are handles on one directory.
//
// It compares filesystem objects, not pathnames, and both are already open, so
// the answer cannot be invalidated afterwards: a rename moves a name, never the
// object a handle holds.
//
// It settles the two directories and nothing else. A false result is not a
// guarantee that a copy between them cannot land on its own source: hard links
// mean two distinct directories can hold one inode, so the files have to be
// compared as well — see the per-file check in internal/memory's repo copy.
// Nor does it say anything about where the directories are relative to each
// other; one can be inside the other and still be a different object.
func (r *Root) SameDir(other *Root) (bool, error) {
	return r.SameDirAt(".", other)
}

// SameDirAt reports whether rel, resolved through r, is the directory other is
// pinned on. SameDir is this with rel of ".".
//
// It exists so a traversal can ask "is this entry the destination?" of every
// directory it is about to descend into, rather than proving once beforehand
// that the destination is elsewhere. A one-time proof is about where things
// were; this is about the entry actually in hand.
func (r *Root) SameDirAt(rel string, other *Root) (bool, error) {
	mine, err := r.root.Stat(rel)
	if err != nil {
		return false, err
	}
	theirs, err := other.root.Stat(".")
	if err != nil {
		return false, err
	}
	return os.SameFile(mine, theirs), nil
}

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
	return s.open(path, nil)
}

// open is Open with a hook that runs immediately after a root is pinned and
// before anything is authorized against it.
//
// The hook is a parameter rather than package state so a test can stage the
// replacement this ordering exists to survive without two parallel tests ever
// seeing each other's hook. It is nil on every production path.
func (s Set) open(path string, afterPin func()) (*Target, error) {
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
		pinned, rootID, err := openIdentified(root, afterPin)
		if err != nil {
			// Absent is not a failure: a root that is not there cannot own the
			// target, and one unavailable root must not disable the others.
			// Every other failure — permission, I/O, an identity that no longer
			// matches the pin — is returned, matching pathid: a root whose state
			// cannot be established is not a root known to be irrelevant.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !rootID.Contains(target) {
			_ = pinned.Close()
			continue
		}
		rel, err := filepath.Rel(rootID.Path(), target.Path())
		if err != nil {
			// Unreachable: Contains has already established a relative route.
			_ = pinned.Close()
			return nil, fmt.Errorf("rootfs: %s relative to %s: %w", target, rootID, err)
		}
		return &Target{root: pinned, rel: rel, display: display}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrOutsideRoots, path)
}

// OpenIdentified pins the directory at path and returns it together with the
// physical identity that the pinned directory has been confirmed to have.
//
// The pairing is the point. An identity resolved separately from a pin is an
// identity of a name, and a caller that then reasons about the handle — is it
// inside that other directory, is it the same as this one — is reasoning about
// whatever the name meant, which need not be what it holds open. Returned
// together, the two are known to describe one directory.
//
// The order is the rest of it. The name is dereferenced exactly once, by the
// open, and everything afterwards is checked against what that open pinned.
// Resolving first and pinning second leaves a window in which the resolved
// directory is renamed aside and another put in its place: the open then pins
// the replacement, and every conclusion drawn from the resolution is about a
// directory that is no longer involved. Resolving after the pin does not close
// that on its own, because the resolution reads the same name an attacker
// controls — the SameFile check is what closes it, and a name that has moved on
// since the pin fails the comparison and refuses the call.
//
// What remains is the case where the name already meant the attacker's
// directory when it was opened. That is not a check/use race: any
// implementation must dereference the name at some instant, and this one
// dereferences it once.
//
// The caller closes the returned Root.
func OpenIdentified(path string) (*Root, pathid.ID, error) {
	return openIdentified(path, nil)
}

// openIdentified is OpenIdentified with a hook that runs in the window between
// the pin and the resolution, so a test can stage the replacement the SameFile
// check exists to catch. The hook is a parameter rather than package state so
// parallel tests cannot see each other's. It is nil on every production path.
func openIdentified(path string, afterPin func()) (*Root, pathid.ID, error) {
	pinned, err := Open(path)
	if err != nil {
		return nil, pathid.ID{}, err
	}
	if afterPin != nil {
		afterPin()
	}
	id, err := pathid.Resolve(path)
	if err != nil {
		_ = pinned.Close()
		return nil, pathid.ID{}, fmt.Errorf("rootfs: cannot resolve root %s: %w", path, err)
	}
	same, err := pinned.matches(id)
	if err != nil {
		_ = pinned.Close()
		return nil, pathid.ID{}, fmt.Errorf("rootfs: cannot confirm root %s: %w", path, err)
	}
	if !same {
		_ = pinned.Close()
		return nil, pathid.ID{}, fmt.Errorf("rootfs: root %s changed while it was being opened", path)
	}
	return pinned, id, nil
}

// matches reports whether the pinned directory is the one id names. Both sides
// are compared as filesystem objects rather than as pathnames, which is the
// only comparison that can tell a pinned directory apart from a replacement
// that has taken over its name.
func (r *Root) matches(id pathid.ID) (bool, error) {
	pinnedInfo, err := r.root.Stat(".")
	if err != nil {
		return false, err
	}
	namedInfo, err := os.Stat(id.Path())
	if err != nil {
		return false, err
	}
	return os.SameFile(pinnedInfo, namedInfo), nil
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

// ReadDir lists the directory's entries, sorted by filename.
//
// The sort is not incidental. os.Root has no ReadDir, so this reads through an
// open handle, and File.ReadDir returns entries in filesystem order — which
// varies by filesystem and by the order files were created. os.ReadDir, which
// the tool layer used before, sorts. Callers render this straight into tool
// output, so unsorted entries would make a directory listing differ between two
// identical calls.
func (t *Target) ReadDir() ([]os.DirEntry, error) {
	entries, err := t.root.ReadDir(t.rel)
	if err != nil {
		return nil, t.pathError(err)
	}
	return entries, nil
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

// CreateExclusive writes data to the target, failing with fs.ErrExist if
// anything is already there.
//
// This is a different operation from WriteAtomic, not a variant of it, and the
// difference is which guarantee is worth more. WriteAtomic publishes by rename,
// which replaces whatever holds the name at that instant — correct for editing
// a file that is meant to exist, and destructive for creating one that is not.
// A create guarded by a preceding existence check is a check/use race: another
// writer that lands between the two loses its file to the rename, silently.
//
// O_EXCL is the atomic no-replace primitive, so creation and the claim on the
// name are one step. The cost is that the write is no longer crash-atomic: an
// interrupted call can leave a short file. That is the right trade for a path
// that held nothing beforehand — a partial new file loses only what this call
// was writing, while a rename loses somebody else's work.
//
// A failed write leaves that partial file rather than cleaning it up, and the
// omission is deliberate. Removing it means removing a name, and by the time
// the handle is closed the name may belong to somebody else's file — so tidying
// up after a failure of ours would delete a file that was never ours. There is
// no portable way to unlink the exact object a handle refers to, and an
// identity check before the remove is the same race one step smaller. The
// documented trade above already permits a short file; it does not permit
// deleting a stranger's.
func (t *Target) CreateExclusive(data []byte, perm fs.FileMode) error {
	f, err := t.root.root.OpenFile(t.rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return t.pathError(err)
	}
	// The mode is applied to the open handle because a umask can reduce the
	// permission requested at create time.
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return t.pathError(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return t.pathError(err)
	}
	if err := f.Close(); err != nil {
		return t.pathError(err)
	}
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
