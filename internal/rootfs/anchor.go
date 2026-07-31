package rootfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Anchor is an identity-bound directory reference.  It retains an open
// kernel handle to the pinned directory so identity comparison with
// os.SameFile is durable — unlike a stored os.FileInfo, a held handle
// prevents the inode from being reused after deletion.
//
// Anchor.Open opens the same pathname fresh, stats both the new handle
// and the pinned handle, compares them with os.SameFile, and returns the
// verified new handle.  The caller closes the returned Root.
//
// The stored pathname is private; there is no pathname accessor.
type Anchor struct {
	root *Root
	path string
}

// NewAnchor pins the directory at path and captures its identity by
// retaining an open handle.  The caller closes the Anchor when done.
func NewAnchor(path string) (*Anchor, error) {
	r, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot open anchor %s: %w", path, err)
	}
	return &Anchor{root: r, path: path}, nil
}

// Close releases the pinned handle.
func (a *Anchor) Close() error { return a.root.Close() }

// SameAnchor reports whether a and other refer to the same filesystem
// directory.  The comparison uses os.SameFile on the two pinned handles
// — no pathname re-resolution is involved.
//
// This is how components that opened two different handles answer "are
// these the same directory."  pathid.Same resolved two pathnames; this
// compares the objects those handles actually refer to.
func (a *Anchor) SameAnchor(other *Anchor) (bool, error) {
	return a.root.SameDir(other.root)
}

// SameRoot reports whether the pinned directory is the same filesystem
// object as r.
func (a *Anchor) SameRoot(r *Root) (bool, error) {
	return a.root.SameDir(r)
}

// Open opens the stored pathname, verifies that the new handle refers to
// the same filesystem object as the pinned handle, and returns the
// verified handle.  The caller closes the returned Root.
func (a *Anchor) Open() (*Root, error) {
	return a.open(nil)
}

// OpenChild opens name as a child of the pinned directory, verifies it is
// not a link, and returns a new Anchor pinned to that child. The caller
// closes the returned Anchor.  The child pathname is derived from the
// parent's stored path so new child Anchors are always contained within
// the parent's directory.
func (a *Anchor) OpenChild(name string) (*Anchor, error) {
	r, err := a.Open()
	if err != nil {
		return nil, err
	}
	child, _, err := r.OpenChildNoFollow(name)
	_ = r.Close()
	if err != nil {
		return nil, err
	}
	childPath := a.path + string(filepath.Separator) + name
	return &Anchor{root: child, path: childPath}, nil
}

// statFn is the signature of Root.Stat or a test replacement.
type statFn func(r *Root) (fs.FileInfo, error)

// open is Open with an optional stat function replacement.  If stat is
// non-nil it is called twice — once for the reopened handle and once for
// the pinned handle — instead of Root.Stat.  A test can use a counter or
// closure to fail either call.
func (a *Anchor) open(stat statFn) (*Root, error) {
	r, err := Open(a.path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot reopen anchor %s: %w", a.path, err)
	}
	s := stat
	if s == nil {
		s = func(r *Root) (fs.FileInfo, error) { return r.root.Stat(".") }
	}
	newInfo, err := s(r)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("rootfs: cannot stat reopened anchor %s: %w", a.path, err)
	}
	pinnedInfo, err := s(a.root)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("rootfs: cannot stat pinned anchor %s: %w", a.path, err)
	}
	if !os.SameFile(pinnedInfo, newInfo) {
		_ = r.Close()
		return nil, fmt.Errorf("rootfs: anchor %s changed since construction", a.path)
	}
	return r, nil
}
