package rootfs

import (
	"fmt"
	"os"
)

// Anchor is an identity-bound, reopenable directory reference.  It stores
// the configured pathname and the filesystem-object identity captured when
// it was first pinned.  Each operation reopens the pathname, compares the
// reopened directory against the stored identity, and acts through that
// fresh handle.  No long-lived os.Root handle is held between operations.
type Anchor struct {
	path string
	info os.FileInfo
}

// NewAnchor opens path and captures the filesystem-object identity of the
// directory it names.  The directory must exist.
func NewAnchor(path string) (*Anchor, error) {
	r, err := Open(path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot open anchor %s: %w", path, err)
	}
	defer r.Close()
	info, err := r.root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot stat anchor %s: %w", path, err)
	}
	return &Anchor{path: path, info: info}, nil
}

// Path returns the configured pathname.
func (a *Anchor) Path() string { return a.path }

// Open reopens the configured directory, verifies that the opened handle
// still refers to the filesystem object captured at construction, and
// returns a Root pinned to that directory.  The caller closes the result.
func (a *Anchor) Open() (*Root, error) {
	r, err := Open(a.path)
	if err != nil {
		return nil, fmt.Errorf("rootfs: cannot reopen anchor %s: %w", a.path, err)
	}
	info, err := r.root.Stat(".")
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("rootfs: cannot stat reopened anchor %s: %w", a.path, err)
	}
	if !os.SameFile(a.info, info) {
		r.Close()
		return nil, fmt.Errorf("rootfs: anchor %s changed since construction", a.path)
	}
	return r, nil
}
