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
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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

// IsNotDirectory reports whether err says a path exists but is not a
// directory, as returned by Open, OpenChild, or Set.Open. Each platform spells
// this its own way: Unix reports ENOTDIR; opening a root on Windows goes
// through the NT layer, which has no ENOTDIR equivalent among the errors
// package syscall exports and is matched by message instead.
//
// A caller using this to choose between two error messages is fine — it only
// picks which one a human reads. A caller that would use it to choose between
// two different *actions* should not: the distinction it draws is between
// "refused" and "refused, and here specifically is why", not between "safe"
// and "unsafe". Open already refused both.
func IsNotDirectory(err error) bool {
	for _, errno := range notDirectoryErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil {
		return pathErr.Err.Error() == "not a directory"
	}
	return false
}

// Close releases the directory handle.
func (r *Root) Close() error { return r.root.Close() }

// ReadFile reads the file at rel, which is resolved through the pinned
// directory.
func (r *Root) ReadFile(rel string) ([]byte, error) { return r.root.ReadFile(rel) }

// Lstat describes rel without following it when it is itself a link.
func (r *Root) Lstat(rel string) (fs.FileInfo, error) { return r.root.Lstat(rel) }

// Stat describes rel, following it when it is a link that stays inside the
// root. A link that leaves the root fails rather than resolving.
//
// It is a read, and only a read. Nothing in this package pairs it with a
// mutation of the same name: stat-by-name followed by mutate-by-name is two
// resolutions of one name, and the operations that mutate — WriteStreamAtomic,
// CreateExclusive, AppendSync — each dereference the name once themselves
// rather than acting on what a preceding Stat found.
func (r *Root) Stat(rel string) (fs.FileInfo, error) { return r.root.Stat(rel) }

// Readlink returns the target rel points at, without resolving it. A link
// whose target lies outside the root is still readable — reading a link is not
// following one, and callers that represent a link by its target (git stores
// one that way) need the value.
func (r *Root) Readlink(rel string) (string, error) { return r.root.Readlink(rel) }

// WriteStreamAtomic writes everything src yields to rel, through a temporary
// file in the same directory that is then renamed over rel. Every step resolves
// through the pinned root.
//
// Publishing by rename replaces the directory *entry* and leaves the inode that
// held the name alone. That is what makes it safe to write into a tree whose
// entries may be hard links to files elsewhere. Opening the destination and
// truncating it writes through the link instead: if the entry is a link to some
// file in the source, that file is emptied — and the obvious guard, comparing
// the pair being copied with os.SameFile, does not see it, because the
// destination entry can be linked to a *different* source file than the one
// being read. Only replacing the entry is safe against a link to anything.
//
// The temporary file is removed unless the rename consumes it, so a failed
// write leaves neither a partial target nor a stray file. Removing it is safe
// where removing a published name would not be: the temporary name was minted
// by this call under a prefix no other writer uses, so it cannot have become
// somebody else's file in the meantime.
//
// The temporary file is fsynced before the rename. Without it "atomic" holds
// only against a concurrent reader, not against a crash: the rename can reach
// the disk while the contents have not, leaving the name published over an
// empty or partial file.
//
// rel's directory is pinned once, before the temp file is created, and every
// later step — the create, the cleanup remove, and the final rename — resolves
// against that one pinned handle rather than against rel's directory component
// a second and third time. Re-resolving it on each call would let a directory
// swapped in between them redirect the cleanup or the rename: the temp file's
// random name is only unpredictable at the moment it is minted, and a reader
// that lists the directory afterwards, moves it aside, and plants a file under
// the same name in a replacement can otherwise make the final rename publish
// that planted content instead of the bytes this call wrote.
func (r *Root) WriteStreamAtomic(rel string, src io.Reader, perm fs.FileMode) error {
	return r.writeStreamAtomicHooked(rel, src, perm, nil)
}

// writeStreamAtomicHooked is WriteStreamAtomic with a hook that runs
// immediately after the destination directory is pinned and before anything
// is created inside it, so a test can stage a replacement of that directory's
// name in exactly the window the pin-once design exists to survive. The hook
// is a parameter rather than package state for the same reason every other
// hook in this package is: two tests setting shared state at once would each
// run the other's hook. It is nil on every production path.
func (r *Root) writeStreamAtomicHooked(rel string, src io.Reader, perm fs.FileMode, afterPin func()) error {
	dirRel, base := filepath.Dir(rel), filepath.Base(rel)
	dir := r
	if dirRel != "." {
		child, err := r.OpenChild(dirRel)
		if err != nil {
			return err
		}
		defer child.Close() //nolint:errcheck // read-only handle; the write below has its own error path
		dir = child
	}
	if afterPin != nil {
		afterPin()
	}

	tmpName, f, err := dir.createTemp(perm)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = dir.root.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := dir.root.Rename(tmpName, base); err != nil {
		return err
	}
	cleanup = false
	return nil
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

// OpenChild pins the directory at rel inside r and returns it as a Root in its
// own right. The caller closes it.
//
// A traversal that means to inspect a subdirectory and then descend into it has
// to hold it, not name it twice. Statting rel and then re-resolving the same rel
// to recurse is two resolutions of one name: between them the directory can be
// renamed aside and another moved into its place, so what was inspected and what
// is entered are different directories. Pinning it once and both inspecting and
// descending through that handle removes the second resolution entirely.
func (r *Root) OpenChild(rel string) (*Root, error) {
	child, err := r.root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	return &Root{root: child}, nil
}

// Clone returns a second, independent handle on the directory this one holds.
//
// It is an open, not a cheap duplicate, and the caller closes it. It exists so
// a component that needs a root for its own lifetime can take one without
// inheriting somebody else's close: re-opening by pathname would resolve the
// name a second time and could pin a different directory, while resolving "."
// through the existing handle can only produce the directory already held.
func (r *Root) Clone() (*Root, error) { return r.OpenChild(".") }

// CreateExclusive writes data to rel, failing with fs.ErrExist if anything
// already holds the name. See Target.CreateExclusive for why this is a distinct
// operation from WriteStreamAtomic rather than a variant of it, and why a
// failed write leaves its partial file rather than removing a name that may by
// then belong to someone else.
func (r *Root) CreateExclusive(rel string, data []byte, perm fs.FileMode) error {
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	// The mode is applied to the open handle because a umask can reduce the
	// permission requested at create time.
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

// OpenAppend opens rel for appending, creating it if it is absent, and returns
// a handle the caller keeps open. It never truncates and never seeks.
//
// It is deliberately not a general OpenFile. Handing callers the flag set would
// put O_TRUNC one argument away from an operation whose entire purpose is that
// it cannot destroy previous records, and the returned handle exposes no way to
// shorten the file either.
func (r *Root) OpenAppend(rel string, perm fs.FileMode) (*AppendFile, error) {
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return nil, err
	}
	return &AppendFile{f: f}, nil
}

// AppendFile is an open append-only file inside a Root. Its whole surface is
// "add to the end", "make it durable", and "let go".
type AppendFile struct {
	f *os.File
}

// Write appends p. O_APPEND puts the position in the kernel's hands, so a
// concurrent writer on another handle cannot make two appends overlap.
func (f *AppendFile) Write(p []byte) (int, error) { return f.f.Write(p) }

// Sync flushes the file to stable storage.
func (f *AppendFile) Sync() error { return f.f.Sync() }

// Close releases the handle.
func (f *AppendFile) Close() error { return f.f.Close() }

// AppendSync appends data to rel and fsyncs it, creating the file if it is
// absent. It never truncates: an append-only log's whole guarantee is that what
// is already there stays there.
func (r *Root) AppendSync(rel string, data []byte, perm fs.FileMode) error {
	f, err := r.OpenAppend(rel, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Remove deletes the entry at rel. A directory must be empty.
func (r *Root) Remove(rel string) error { return r.root.Remove(rel) }

// RemoveAll deletes rel and everything below it, resolving each level through
// the root rather than by pathname. A missing rel is not an error.
func (r *Root) RemoveAll(rel string) error { return r.root.RemoveAll(rel) }

// OpenRead opens rel for reading and returns a handle. It does not hand back a
// raw *os.File, whose Name reports the root path joined with rel — an
// authorized read that a caller could turn back into an unauthorized reopen by
// pathname. There is no read-write or mutating variant: a handle obtained here
// cannot be used to overwrite or truncate what it reads, which is what keeps it
// safe to hand to a caller that only asked to read.
func (r *Root) OpenRead(rel string) (*File, error) {
	f, err := r.root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return &File{f: f}, nil
}

// File is an open, read-only file inside a Root. It carries only the read
// operations a caller needs and nothing that would let it leave the handle:
// there is no accessor for the file's pathname, because an authorized handle
// must not be convertible into a pathname to reopen later, and no mutating
// method, because in-place mutation is exactly the operation this package
// avoids — see WriteStreamAtomic's doc comment for why opening a destination
// read-write and truncating or appending into it can write through a hard link
// to a file the caller never named. A caller that needs to change a file's
// contents publishes a new version by rename instead.
type File struct {
	f *os.File
}

// Size reports the current length, measured through the handle rather than by
// statting the name it was opened under.
func (f *File) Size() (int64, error) {
	info, err := f.f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) { return f.f.Read(p) }

// ReadAt implements io.ReaderAt.
func (f *File) ReadAt(p []byte, off int64) (int, error) { return f.f.ReadAt(p, off) }

// Close releases the handle.
func (f *File) Close() error { return f.f.Close() }

// WalkEntry is one entry reported by Walk.
type WalkEntry struct {
	// Rel is the entry's path relative to the directory the walk started
	// from, with OS separators.
	Rel string
	// Name is the entry's final component.
	Name string
	// Info describes the entry without following it when it is itself a
	// link, resolved through the pinned handle on its parent directory.
	Info fs.FileInfo
}

// WalkFunc is called once per entry found by Walk. Returning skip for a
// directory prunes it; for anything else skip is ignored. A returned error
// stops the walk and is passed back to the caller.
type WalkFunc func(entry WalkEntry) (skip bool, err error)

// maxWalkDepth bounds descent. os.Root follows a symbolic link whose target
// stays inside the root, so a link inside a tree that points at one of its own
// ancestors is a cycle the walk would otherwise follow forever. A bound turns
// that into a diagnosable failure rather than a hang.
const maxWalkDepth = 64

// Walk visits every entry below rel, depth-first, in filename order within each
// directory.
//
// A subtree is pinned once and everything about it — the metadata reported to
// the visitor and the descent into it — comes from that one handle. The
// alternative — describing an entry by name, then resolving the same name
// again to recurse — dereferences the name twice, and between the two calls
// the directory that was described can be renamed aside and a different one
// put in its place, so what is reported and what is entered disagree. Pinning
// before describing closes that: whatever OpenChild pins is what Stat then
// describes and what the recursive call then reads, with no second
// resolution in between.
//
// A directory entry's *type*, from the initial listing, is what decides
// whether to attempt that pin at all. A symbolic link present at listing time
// (a distinct type, even when it targets a directory) is never mistaken for a
// directory: it is Lstat'd and reported without an OpenChild attempt. That
// listing is a snapshot, though, and os.Root follows an in-root symlink rather
// than refusing it — so a name that was a real directory when listed and
// becomes a symlink before the pin runs would be opened successfully by
// OpenChild if that were the only check. It is not: walkDir Lstat's the same
// name through the parent immediately after pinning it, which does not follow
// links, and refuses to describe or enter the pinned child unless that Lstat
// still shows a directory. "A link is reported as a link and not descended
// into" is enforced by type at listing time and re-checked at the pin, not by
// type at listing time alone. An entry that disappears, or changes to
// something OpenChild refuses, between the listing and the pin is skipped
// rather than treated as an error — it is gone or has become something this
// walk does not enter, which is a valid answer to "what is here", not a
// failure.
func (r *Root) Walk(rel string, visit WalkFunc) error {
	start := r
	if rel != "" && rel != "." {
		child, err := r.OpenChild(rel)
		if err != nil {
			return err
		}
		defer child.Close() //nolint:errcheck // read-only handle
		start = child
	}
	return start.walk("", visit, 0)
}

func (r *Root) walk(prefix string, visit WalkFunc, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("rootfs: tree nests deeper than %d levels at %q — refusing to continue", maxWalkDepth, prefix)
	}
	entries, err := r.ReadDir(".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		relPath := name
		if prefix != "" {
			relPath = filepath.Join(prefix, name)
		}
		if entry.IsDir() {
			if err := r.walkDir(name, relPath, visit, depth); err != nil {
				return err
			}
			continue
		}
		// Not a directory per the listing's own type bits — a file, or a
		// symbolic link even when its target is a directory. Nothing will be
		// entered, so a plain Lstat by name is both correct and sufficient.
		info, err := r.root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if _, err := visit(WalkEntry{Rel: relPath, Name: name, Info: info}); err != nil {
			return err
		}
	}
	return nil
}

// walkDir pins one subdirectory, describes it through that same handle, and —
// unless the visitor skips it — descends through it. name is only ever passed
// here when the initial listing already reported it as a directory.
//
// That listing is a snapshot, not a live fact: os.Root follows an in-root
// symlink rather than refusing it, so a name that was a real directory when
// listed and becomes an in-root symlink before this call runs is still opened
// successfully by OpenChild — the pin alone does not tell descend and describe
// apart from follow-a-link-that-only-just-appeared. What closes that is the
// Lstat immediately below: it names the same "name" through the parent, which
// does not follow links, so it reports the entry as it is *now*, at the moment
// this is about to be treated as a directory — not as the earlier listing
// recorded it. A symlink there, however it got there, is reported as a link
// through the parent's own description and never entered, matching how a
// symlink present at listing time is handled by the caller's non-directory
// branch.
func (r *Root) walkDir(name, relPath string, visit WalkFunc, depth int) error {
	child, err := r.OpenChild(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// The entry stopped being an openable directory between the listing
		// and this call — replaced by a file, or by a link leaving the root,
		// both of which OpenChild refuses. Either way there is nothing here
		// this walk can enter, which is the same "it is gone" case as
		// ErrNotExist from this walk's point of view.
		if IsNotDirectory(err) {
			return nil
		}
		return err
	}

	parentInfo, err := r.root.Lstat(name)
	if err != nil {
		_ = child.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if parentInfo.Mode()&fs.ModeSymlink != 0 {
		_ = child.Close()
		_, err := visit(WalkEntry{Rel: relPath, Name: name, Info: parentInfo})
		return err
	}
	defer child.Close() //nolint:errcheck // read-only handle

	info, err := child.root.Stat(".")
	if err != nil {
		return err
	}
	skip, err := visit(WalkEntry{Rel: relPath, Name: name, Info: info})
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	return child.walk(relPath, visit, depth+1)
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

// open is Open with two hooks, each nil on every production path: afterPin
// runs immediately after a root is pinned and before anything is authorized
// against it (inside openIdentified, between its own pin and its own
// identity check); afterRootVerify runs once that root has been pinned *and*
// verified, immediately before the target is resolved against it.
//
// The hooks are parameters rather than package state so a test can stage a
// replacement in one of these windows without two parallel tests ever seeing
// each other's hook.
//
// The target is resolved fresh inside the loop, once per candidate root,
// rather than once before the loop starts. Resolving it once up front and then
// comparing it against roots pinned afterwards would authorize a target
// identity that predates every root's own pin: a root swapped out between that
// single resolution and a later iteration's pin is then judged against a
// target identity that is stale relative to it, rather than against one taken
// close to the same moment. Resolving the target again for each root keeps the
// two readings adjacent in time for whichever root ultimately matches, which
// is the one comparison that is authorized.
//
// Narrowing that window does not, on its own, bound what a Target ends up
// reaching — os.Root does that. Whatever rootID.Contains admits still has to
// name something reachable through pinned's own os.Root handle at rel, and
// os.Root re-resolves rel from that handle at the moment of the actual read
// or write, independent of anything Contains decided earlier. A root repointed
// in the afterRootVerify window changes what target resolves to, but Contains
// then requires that new target to sit inside rootID's *already-fixed*
// identity — the root that was pinned and verified before the repoint — so a
// replacement that is not itself nested inside that root is refused outright,
// and the one case where containment could still pass (the replacement
// happens to be reachable from inside the pinned root already) reads or
// writes only inside that same pinned root, never outside it. See
// TestSetOpenTargetRepointedAfterRootVerifyStaysBoundToThePinnedRoot.
func (s Set) open(path string, afterPin func()) (*Target, error) {
	return s.openHooked(path, afterPin, nil)
}

func (s Set) openHooked(path string, afterPin func(), afterRootVerify func()) (*Target, error) {
	display, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot make %s absolute: %w", path, err)
	}
	for _, root := range s {
		if strings.TrimSpace(root) == "" {
			continue
		}
		pinned, rootID, err := openIdentified(root, afterPin)
		if afterRootVerify != nil {
			afterRootVerify()
		}
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
	if err := t.root.CreateExclusive(t.rel, data, perm); err != nil {
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

// createTemp opens a new, uniquely named file directly inside r.
//
// os.CreateTemp cannot be used here: it takes a pathname, which is exactly
// what the root exists to keep out of the decision. The retry loop plays the
// same role as its own — O_EXCL makes the create fail rather than truncate if
// the name is taken, so a collision costs another attempt and never someone
// else's file.
//
// The caller is r itself — the destination directory, already pinned — rather
// than a directory named relative to some other root. Naming the destination
// separately from opening it is exactly the pattern this method exists to
// avoid: whatever r holds open is where the temp file lands, with no second
// resolution of a directory path in between.
func (r *Root) createTemp(perm fs.FileMode) (string, *os.File, error) {
	const attempts = 1000
	for range attempts {
		name := tempNamePrefix + rand.Text()
		f, err := r.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
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
			_ = r.root.Remove(name)
			return "", nil, err
		}
		return name, f, nil
	}
	return "", nil, fmt.Errorf("rootfs: no free temporary name after %d attempts", attempts)
}
