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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/VrncQuentin/harness/internal/pathid"
)

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

// OpenOrCreate pins the directory at path, creating it and any missing
// ancestors first when they do not exist.
//
// The creation is itself rooted: the deepest existing ancestor is pinned and
// the missing tail is created through that handle, so no pathname is ever
// handed to a direct os.MkdirAll. The directory must still be under some
// existing directory — the harness home skeleton, which internal/home creates
// at startup — so the ancestor search is bounded and fails closed.
//
// The caller closes the result. The returned Root is pinned on the created
// (or already-present) directory itself.
func OpenOrCreate(path string, perm fs.FileMode) (*Root, error) {
	path = filepath.Clean(path)
	pinned, err := Open(path)
	if err == nil {
		return pinned, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("rootfs: open root %s: %w", path, err)
	}

	// Find the deepest existing ancestor by probing opens. An open of a path
	// with a missing component fails with fs.ErrNotExist, so the first open
	// that succeeds is the nearest ancestor we can pin.
	ancestor := filepath.Dir(path)
	for {
		existing, openErr := Open(ancestor)
		if openErr == nil {
			defer func() { _ = existing.Close() }()
			rel, relErr := filepath.Rel(ancestor, path)
			if relErr != nil {
				return nil, fmt.Errorf("rootfs: %s relative to %s: %w", path, ancestor, relErr)
			}
			if rel != "." {
				if mkErr := existing.MkdirAll(filepath.FromSlash(rel), perm); mkErr != nil {
					return nil, fmt.Errorf("rootfs: create %s: %w", path, mkErr)
				}
			}
			child, childErr := existing.OpenChild(filepath.FromSlash(rel))
			if childErr != nil {
				return nil, fmt.Errorf("rootfs: open %s: %w", path, childErr)
			}
			return child, nil
		}
		if !errors.Is(openErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("rootfs: open ancestor %s: %w", ancestor, openErr)
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return nil, fmt.Errorf("rootfs: no existing ancestor for %s: %w", path, err)
		}
		ancestor = next
	}
}

// Close releases the directory handle.
func (r *Root) Close() error { return r.root.Close() }

// SameDir reports whether r and other are handles on one directory.
//
// It compares filesystem objects, not pathnames, and both are already open, so
// the answer cannot be invalidated afterwards: a rename moves a name, never the
// object a handle holds.
//
// It settles the two directories and nothing else. It says nothing about the
// files inside them, which can be hard links to files anywhere — the repo copy
// handles that by replacing destination entries rather than writing through
// them. Nor does it say anything about where the directories are relative to
// each other; one can be inside the other and still be a different object.
func (r *Root) SameDir(other *Root) (bool, error) {
	mine, err := r.root.Stat(".")
	if err != nil {
		return false, err
	}
	theirs, err := other.root.Stat(".")
	if err != nil {
		return false, err
	}
	return os.SameFile(mine, theirs), nil
}

// Identity returns the physical identity of the directory at dir, verified
// against this pinned handle.
//
// Resolving the name independently of the handle would let a repointed alias
// hand back a key for a directory this handle does not hold open: a coordinator
// keyed on that identity would serialize writers on one object while the I/O
// acted on another.  The identity is therefore accepted only if it resolves to
// the same filesystem object as this handle — a name that has moved since the
// pin fails the comparison and the call fails closed.
func (r *Root) Identity(dir string) (pathid.ID, error) {
	verified, id, err := OpenIdentified(dir)
	if err != nil {
		return pathid.ID{}, err
	}
	defer func() { _ = verified.Close() }()
	same, err := r.SameDir(verified)
	if err != nil {
		return pathid.ID{}, err
	}
	if !same {
		return pathid.ID{}, fmt.Errorf("rootfs: %s does not identify the pinned directory", dir)
	}
	return id, nil
}

// OpenIdentifiedHooked is OpenIdentified with a hook that runs in the window
// between the pin and the identity resolution, so a test can stage the
// replacement the SameFile check exists to catch. The hook is a parameter
// rather than package state so parallel tests cannot see each other's. It is
// nil on every production path.
func OpenIdentifiedHooked(path string, afterPin func()) (*Root, pathid.ID, error) {
	return openIdentified(path, afterPin)
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
