package rootfs

import (
	"bytes"
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

// Set is an ordered list of root directories. It converts a caller-supplied
// path — absolute or relative, in any spelling — into an operation performed
// inside whichever root physically owns it.
type Set []string

// Open resolves path against the set and pins the root that owns it.
//
// The owning root is chosen by physical identity, and the containment decision
// is made alongside each pinned candidate rather than against a resolution
// taken before any root was pinned. Resolving the target once up front, before
// the loop, leaves the decision and the retained handle describing different
// instants: a root pinned later is judged against a target resolution taken
// earlier, so what was decided and what is held open can disagree. Resolving
// the target right after each pin means the containment verdict and the open
// handle describe the same boundary.
//
// A path reaching a root through a link, a junction, an 8.3 alias, or a
// different case on Windows is recognised as being inside it, and a path that
// leaves a root through one of those is not. Resolution failure on either side
// rejects the call: an unlocatable path is not a path known to be safe.
//
// The caller closes the returned Target.
func (s Set) Open(path string) (*Target, error) {
	return s.open(path, nil)
}

// open is Open with a hook that runs immediately after a root is pinned and
// before the target is resolved against it.
//
// The hook is a parameter rather than package state so a test can stage the
// replacement this ordering exists to survive without two parallel tests ever
// seeing each other's hook. It is nil on every production path.
func (s Set) open(path string, afterPin func()) (*Target, error) {
	display, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot make %s absolute: %w", path, err)
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
		// Resolve the target alongside this pin, so the containment decision
		// and the retained handle describe the same instant.
		target, err := pathid.Resolve(display)
		if err != nil {
			_ = pinned.Close()
			return nil, fmt.Errorf("rootfs: cannot resolve %s: %w", path, err)
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
	if err := t.root.WriteStreamAtomic(t.rel, bytes.NewReader(data), perm); err != nil {
		return t.pathError(err)
	}
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
