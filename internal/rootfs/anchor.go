package rootfs

import (
	"fmt"
	"io/fs"
	"os"
)

// Anchor is an identity-bound directory reference.  It retains an open
// kernel handle to the pinned directory so identity comparison with
// os.SameFile is durable — unlike a stored os.FileInfo, a held handle
// prevents the inode from being reused after deletion.
//
// Anchor.Open opens the same pathname fresh, stat's both the new handle
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

// Open opens the stored pathname, verifies that the new handle refers to
// the same filesystem object as the pinned handle, and returns the
// verified handle.  The caller closes the returned Root.
func (a *Anchor) Open() (*Root, error) {
	return a.open(nil, nil)
}

// statFn is the signature of Root.Stat or a test replacement.
type statFn func(r *Root) (fs.FileInfo, error)

// open is Open with optional stat-replacement functions for each identity
// check.  If reopenedStat is nil, r.root.Stat(".") is used for the
// reopened handle.  If pinnedStat is nil, a.root.root.Stat(".") is used
// for the pinned handle.
func (a *Anchor) open(reopenedStat, pinnedStat statFn) (*Root, error) {
	r, err := Open(a.path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot reopen anchor %s: %w", a.path, err)
	}
	rs := reopenedStat
	if rs == nil {
		rs = func(r *Root) (fs.FileInfo, error) { return r.root.Stat(".") }
	}
	newInfo, err := rs(r)
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("rootfs: cannot stat reopened anchor %s: %w", a.path, err)
	}
	ps := pinnedStat
	if ps == nil {
		ps = func(r *Root) (fs.FileInfo, error) { return r.root.Stat(".") }
	}
	pinnedInfo, err := ps(a.root)
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
