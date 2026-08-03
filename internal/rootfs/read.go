package rootfs

import (
	"io/fs"
	"os"
	"slices"
	"strings"
)

// ReadFile reads the file at rel, which is resolved through the pinned
// directory.
func (r *Root) ReadFile(rel string) ([]byte, error) { return r.root.ReadFile(rel) }

// Lstat describes rel without following it when it is itself a link.
func (r *Root) Lstat(rel string) (fs.FileInfo, error) { return r.root.Lstat(rel) }

// Stat describes rel, following a final symlink whose target stays inside the
// root. A link that cannot stay inside the root is refused, which is what makes
// Stat safe to use for existence checks on entries the root must keep confined.
func (r *Root) Stat(rel string) (fs.FileInfo, error) { return r.root.Stat(rel) }

// Readlink returns the target rel points at, without resolving it. A link
// whose target lies outside the root is still readable — reading a link is not
// following one, and callers that represent a link by its target (git stores
// one that way) need the value.
func (r *Root) Readlink(rel string) (string, error) { return r.root.Readlink(rel) }

// ReadCloser is a read-only handle over a file opened through a pinned root.
//
// It deliberately hides the underlying *os.File. A file handle's Name()
// reveals the pathname it was opened through, which is exactly what the root
// exists to keep out of a caller's hands: a caller holding that name could
// reopen the file by pathname, and the reopened handle would not be bounded by
// the root. ReadCloser exposes Read and Close and nothing else, so the only way
// to read this file is through the handle that was pinned.
type ReadCloser struct {
	f *os.File
}

// Read reads from the pinned file.
func (rc *ReadCloser) Read(p []byte) (int, error) { return rc.f.Read(p) }

// Close releases the pinned file handle.
func (rc *ReadCloser) Close() error { return rc.f.Close() }

// OpenRead opens rel for reading and returns it as an opaque ReadCloser. The
// caller closes it. Unlike an *os.File, the returned handle exposes no
// pathname, so a read can never become a pathname reopen.
func (r *Root) OpenRead(rel string) (*ReadCloser, error) {
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, err
	}
	return &ReadCloser{f: f}, nil
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
