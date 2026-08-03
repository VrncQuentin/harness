package rootfs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteHooks are the seams threaded into WriteStreamAtomicWithHooks.  The
// fields run at the lifecycle points they name:
//
//   - AfterOpen runs after the temp file is created and before its content is
//     copied.  It receives the open handle and the temp-relative path.
//   - Sync, if non-nil, replaces f.Sync().
//   - Rename, if non-nil, replaces the rename that publishes the temp file.  A
//     non-nil error aborts the publication leaving the destination untouched —
//     the same state a failed rename leaves, so any error handling around a
//     failed rename runs against it.
//   - AfterRename runs after the rename has consumed the temp name, before the
//     write returns.  A test can claim the vacated name here and prove that a
//     cleanup-by-name after the rename would delete a stranger.
//
// Production callers use WriteStreamAtomic, which passes no hooks; the hooks
// exist so regression tests outside this package can stage failures and
// substitutions at the real lifecycle points instead of replacing the writer.
type WriteHooks struct {
	AfterOpen   func(f *os.File, tmpRel string)
	Sync        func(f *os.File) error
	Rename      func(tmpRel, base string) error
	AfterRename func(tmpRel string)
}

// WriteStreamAtomic writes everything src yields to rel, through a temporary
// file in the same directory that is then renamed over rel.
//
// The destination parent directory is pinned once with OpenChild, so every
// step — temp creation, fsync, identity verification, rename — acts on the
// same directory.  An intermediate-directory swap cannot redirect later steps.
//
// Publishing by rename replaces the directory *entry* and leaves the inode that
// held the name alone.  That is what makes it safe to write into a tree whose
// entries may be hard links to files elsewhere.
//
// The data is fsynced before the rename so a crash does not leave the
// destination half-written.  The temporary file's identity is captured from the
// open handle before f.Close() and compared with the named entry through the
// pinned parent directory via os.SameFile — a substituted entry at the
// temporary name is therefore refused.
//
// An external process can still substitute the temporary entry between that
// identity check and the rename.  Closing that window requires a
// compare-and-rename primitive on a handle, which no portable Go standard
// library primitive provides.
//
// On failure, the temporary file is NOT removed: removing a name whose
// ownership may have changed since it was created is unsafe, and portable
// Go has no unlink-by-handle primitive.  A failed write may leave a partial
// temporary entry.
func (r *Root) WriteStreamAtomic(rel string, src io.Reader, perm fs.FileMode) error {
	return r.writeStreamAtomic(rel, src, perm, WriteHooks{})
}

// WriteStreamAtomicWithHooks is WriteStreamAtomic with the WriteHooks seams
// enabled.  It is exported so regression tests outside this package can inject
// failures at the real sync and rename points; production code uses
// WriteStreamAtomic.
func (r *Root) WriteStreamAtomicWithHooks(rel string, src io.Reader, perm fs.FileMode, hooks WriteHooks) error {
	return r.writeStreamAtomic(rel, src, perm, hooks)
}

func (r *Root) writeStreamAtomic(rel string, src io.Reader, perm fs.FileMode, hooks WriteHooks) error {
	parentDir := filepath.Dir(rel)
	parent, err := r.OpenChild(parentDir)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	base := filepath.Base(rel)
	tmpRel, f, err := parent.createTemp(".", perm)
	if err != nil {
		return err
	}

	if hooks.AfterOpen != nil {
		hooks.AfterOpen(f, tmpRel)
	}

	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return err
	}

	sf := hooks.Sync
	if sf == nil {
		sf = func(f *os.File) error { return f.Sync() }
	}
	if err := sf(f); err != nil {
		_ = f.Close()
		return err
	}

	tmpHandleInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	tmpNameInfo, err := parent.root.Stat(tmpRel)
	if err != nil {
		_ = f.Close()
		return err
	}
	if !os.SameFile(tmpHandleInfo, tmpNameInfo) {
		_ = f.Close()
		return fmt.Errorf("rootfs: temporary entry %s was substituted", tmpRel)
	}

	if err := f.Close(); err != nil {
		return err
	}
	if hooks.Rename != nil {
		if err := hooks.Rename(tmpRel, base); err != nil {
			return err
		}
	} else if err := parent.root.Rename(tmpRel, base); err != nil {
		return err
	}
	if hooks.AfterRename != nil {
		hooks.AfterRename(tmpRel)
	}
	return nil
}

// MkdirAll creates rel and any missing parents inside the root.
func (r *Root) MkdirAll(rel string, perm fs.FileMode) error {
	return r.root.MkdirAll(rel, perm)
}

// CreateExclusive creates rel, failing with fs.ErrExist if anything already
// holds the name. Creation and the claim on the name are one O_EXCL step, so
// an entry that appears concurrently is never overwritten. It is the Root
// counterpart of Target.CreateExclusive; see there for why a failed exclusive
// create leaves its partial file behind rather than removing it by name.
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
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// appendHooks are the seams threaded into appendSync. The fields run at the
// lifecycle points they name:
//
//   - Write, if non-nil, replaces the single f.Write(data) that appends the
//     record.
//   - Sync, if non-nil, replaces f.Sync().
//
// The hooks are unexported on purpose: they receive the live append target as
// an *os.File, which can truncate, seek, or rewrite — exactly what the
// append-only contract forbids. Keeping them package-private means only this
// package's own tests can reach that handle; every production caller is left
// with AppendSync, whose open is fixed to O_WRONLY|O_CREATE|O_APPEND.
type appendHooks struct {
	Write func(f *os.File, data []byte) error
	Sync  func(f *os.File) error
}

// AppendSync appends data to rel, creating the file if it is absent, and
// fsyncs before returning. It is the narrow append primitive for append-only
// logs such as sessions.jsonl.
//
// The open is rooted, with the flag surface fixed to O_WRONLY|O_CREATE|O_APPEND
// and nothing else: no O_TRUNC, no O_RDWR, no seek, no caller-supplied flags.
// An append-only log's whole guarantee is that what is already there stays
// there, so no truncation-capable spelling of the open exists here for a
// caller to reach. The complete record is appended with one write, the file is
// synced before success, and write, sync, and close failures all propagate.
// A failed append never attempts rollback or cleanup by name: the file it was
// appending to is not removed and its existing contents are not shortened,
// because removing or replacing a name whose ownership may have changed since
// it was opened is unsafe.
//
// The parent directory is not created here — the caller (memory.DirReader)
// creates missing parents through the same root before appending.
//
// Append writes in place, which is inherent to the operation and worth saying
// plainly: unlike WriteStreamAtomic's rename publication, an append cannot
// replace the directory entry, so a sessions.jsonl entry that is a hard link
// to a file elsewhere is written through — the same underlying file gains the
// record. Rooted access prevents pathname, symlink, and junction escapes, but
// it cannot distinguish a hard-linked entry from the same file outside the
// repo. That limitation is inherent to appending in place and is not worked
// around with a read-modify-rename; doing that would replace the audit log's
// append-only identity with a rewrite on every save.
func (r *Root) AppendSync(rel string, data []byte, perm fs.FileMode) error {
	return r.appendSync(rel, data, perm, appendHooks{})
}

// appendSync is AppendSync with the appendHooks seams enabled. It is
// unexported because the only callers are this package's tests; the production
// surface is AppendSync alone, so no caller outside rootfs can turn the live
// append target into a truncate, seek, or rewrite.
func (r *Root) appendSync(rel string, data []byte, perm fs.FileMode, hooks appendHooks) error {
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	wf := hooks.Write
	if wf == nil {
		wf = func(f *os.File, data []byte) error {
			_, err := f.Write(data)
			return err
		}
	}
	if err := wf(f, data); err != nil {
		_ = f.Close()
		return err
	}
	sf := hooks.Sync
	if sf == nil {
		sf = func(f *os.File) error { return f.Sync() }
	}
	if err := sf(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// AppendFile is an append-only handle over a file opened through a pinned
// root.
//
// It hides the underlying *os.File: a file handle's Name() reveals the
// pathname it was opened through, which is exactly what the root exists to
// keep out of a caller's hands. The open is fixed to O_WRONLY|O_CREATE|O_APPEND
// and nothing else, so a caller can append and nothing more — no truncate, no
// seek, no rewrite. Unlike AppendSync there is no per-write fsync; callers that
// need each record durable before success use AppendSync instead.
type AppendFile struct {
	f *os.File
}

// Write appends data to the file.
func (a *AppendFile) Write(data []byte) error {
	_, err := a.f.Write(data)
	return err
}

// Close releases the file handle.
func (a *AppendFile) Close() error { return a.f.Close() }

// OpenAppend opens rel for appending through the pinned root, creating it if
// absent, and returns it as an opaque AppendFile. The caller closes it. The
// open carries O_WRONLY|O_CREATE|O_APPEND and nothing else.
func (r *Root) OpenAppend(rel string, perm fs.FileMode) (*AppendFile, error) {
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_APPEND, perm)
	if err != nil {
		return nil, err
	}
	return &AppendFile{f: f}, nil
}

// RemoveAll removes rel and any children it contains.
func (r *Root) RemoveAll(rel string) error { return r.root.RemoveAll(rel) }

// RemoveVerified removes rel only if the entry there still refers to the same
// filesystem object as observed.
//
// observed must be the FileInfo of the entry captured earlier through this
// same root — for example the FileInfo of a directory entry returned by
// ReadDir. Between the observation and the removal the name may have been
// claimed by a different file (a replacement, a hard link, a re-pointed
// alias); removing by name in that case would delete a stranger's object.
// The entry is re-read through the pinned root and compared with os.SameFile,
// so a name that no longer identifies what was observed is refused rather
// than removed.
//
// The comparison narrows the window but does not close it: an entry swapped
// between this re-read and the remove is still removed. That residual window
// is inherent to any remove-by-name operation in portable Go; the verification
// here is what turns a retention sweep that would delete any old-named entry
// into one that only deletes the entry it actually observed.
func (r *Root) RemoveVerified(rel string, observed fs.FileInfo) error {
	now, err := r.root.Lstat(rel)
	if err != nil {
		return err
	}
	if !os.SameFile(observed, now) {
		return fmt.Errorf("rootfs: %s changed since it was observed; refusing to remove", rel)
	}
	return r.root.Remove(rel)
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
			// Do not remove the temp name — ownership may have
			// changed since creation, and portable Go has no
			// unlink-by-handle primitive.
			return "", nil, err
		}
		return rel, f, nil
	}
	return "", nil, fmt.Errorf("rootfs: no free temporary name in %s after %d attempts", dir, attempts)
}
