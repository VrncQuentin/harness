// Package pathid establishes the physical identity of a filesystem path.
//
// Two questions in this repository need the same answer and must not answer it
// differently: "is this path inside that root" (the tool sandbox and the C2
// memory-repo lock) and "are these two paths the same repository" (the git
// write lock). Both are security or correctness boundaries, and both were
// previously built on filepath.EvalSymlinks, which cannot express identity on
// Windows — see Canonical.
//
// The package answers with an opaque ID rather than a string. A string result
// carries a precondition the caller has to remember — "this one has already
// been resolved" — and the whole class of bugs this package exists to remove
// starts with comparing a resolved path against an unresolved one. An ID can
// only be produced by Resolve, so a comparison between two of them is a
// comparison between two physical locations by construction.
//
// Every function here fails closed. Resolution errors are returned, never
// converted into a lexical guess, because a caller cannot tell an unresolvable
// path from a safe one.
package pathid

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
)

// ID is the resolved physical identity of a path: where it is, or where it
// would be if it were created. The zero value identifies nothing and is
// contained by nothing.
//
// Only Resolve produces a non-zero ID, so every comparison between IDs
// compares two paths that have both been through physical resolution.
//
// Compare with Equal or Contains, and key maps and locks with Key. Do not use
// == or an ID as a map key directly: Go compares every field, including the
// display path, and two IDs can be Equal while their paths differ — Resolve
// re-appends a not-yet-created tail in whatever case the caller spelled it, so
// on Windows the same missing file addressed two ways yields one key and two
// paths.
type ID struct {
	// path is absolute and physically resolved.
	path string
	// key is path reduced to a comparison key — lowercased on Windows, where
	// the filesystem is case-insensitive. It, and not path, is what identity
	// is decided on.
	key string
}

// Path returns the absolute physical path. It is suitable for opening a file
// or for display; it is not suitable for comparison — use Equal or Contains,
// which apply the platform's case rules.
func (id ID) Path() string { return id.path }

// Key returns a comparison key for the identity, for use where a map key or a
// lock key is needed rather than a comparison. It is the only correct way to
// key on an ID; the struct itself is not a sound map key. See the type doc.
func (id ID) Key() string { return id.key }

// String implements fmt.Stringer.
func (id ID) String() string { return id.path }

// IsZero reports whether id identifies nothing.
func (id ID) IsZero() bool { return id.path == "" }

// Equal reports whether id and other are the same physical location.
func (id ID) Equal(other ID) bool {
	if id.IsZero() || other.IsZero() {
		return false
	}
	return id.key == other.key
}

// Contains reports whether other is id itself or lies below it.
//
// Containment is decided by filepath.Rel rather than by a string prefix. A
// prefix test gets three cases wrong that this package has to get right:
//
//   - A filesystem or volume root. "C:\" and "/" already end in a separator,
//     so root+separator is "C:\\" or "//" and nothing below the root matches.
//     Every path on a drive configured as a sandbox root was rejected.
//   - A sibling sharing a prefix. Comparing without the separator accepts
//     "/srv/project-other" as being inside "/srv/project".
//   - Different volumes. "C:\a" and "D:\a" have no containment relationship at
//     all, and Rel reports that by failing rather than by returning a path.
//
// Rel also applies the platform's case rules to each component, matching the
// case-insensitivity this package already assumes on Windows.
func (id ID) Contains(other ID) bool {
	if id.IsZero() || other.IsZero() {
		return false
	}
	rel, err := filepath.Rel(id.path, other.path)
	if err != nil {
		// No relative route exists — different volumes, or one path is UNC and
		// the other is not. That is a definitive "not contained", not a
		// resolution failure: both sides are already resolved.
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// Resolve returns the physical identity of path, canonicalizing the deepest
// component that exists and re-appending the components below it. A relative
// path is made absolute against the working directory first, so the result is
// absolute on every platform — filepath.EvalSymlinks preserves relativity, and
// a relative result compares against nothing.
//
// Re-appending the tail is what makes containment checks sound: returning only
// the existing ancestor would compare a path shorter than the request, so a
// not-yet-created target below an escaping link would be judged by its in-root
// parent rather than its real destination.
//
// Only a genuinely missing component walks upward. Any other failure —
// permission denied, an I/O error, an invalid name — is returned. Walking past
// those would evaluate an existing but unreadable reparse point as though it
// were absent and hand back a lexical path that was never canonicalized, which
// is precisely the fail-open case the sandbox and the C2 lock must not have.
func Resolve(path string) (ID, error) {
	return resolveWith(Canonical, path)
}

// resolveWith is Resolve with an injectable canonicalizer, so the error-kind
// policy can be tested without depending on platform-specific ways to make a
// path unreadable.
func resolveWith(canonical func(string) (string, error), path string) (ID, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return ID{}, fmt.Errorf("pathid: resolve %s: %w", path, err)
	}
	var tail []string
	for {
		resolved, cErr := canonical(current)
		if cErr == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			// A canonicalizer is free to hand back a relative path —
			// EvalSymlinks does when its input is relative — and the loop's
			// input is absolute only on the first iteration of a fresh call.
			// Making the result absolute here keeps that an internal detail.
			absolute, aErr := filepath.Abs(resolved)
			if aErr != nil {
				return ID{}, fmt.Errorf("pathid: resolve %s: %w", path, aErr)
			}
			return newID(absolute), nil
		}
		if !errors.Is(cErr, fs.ErrNotExist) {
			return ID{}, fmt.Errorf("pathid: resolve %s: %w", path, cErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ID{}, fmt.Errorf("pathid: resolve %s: %w", path, cErr)
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

// newID builds an ID from an absolute physical path.
func newID(path string) ID {
	clean := filepath.Clean(path)
	key := clean
	if runtime.GOOS == "windows" {
		key = strings.ToLower(clean)
	}
	return ID{path: clean, key: key}
}

// Same reports whether a and b are the same physical location. Both sides are
// resolved; a failure on either is returned rather than reported as "not the
// same", because an unlocatable path is not a path known to be elsewhere.
func Same(a, b string) (bool, error) {
	idA, err := Resolve(a)
	if err != nil {
		return false, err
	}
	idB, err := Resolve(b)
	if err != nil {
		return false, err
	}
	return idA.Equal(idB), nil
}

// SameOrWithin reports whether path is inside any of roots, resolving both
// sides. Blank roots are skipped. A resolution failure on either side is
// returned rather than treated as "not inside".
func SameOrWithin(path string, roots []string) (bool, error) {
	target, err := Resolve(path)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		rootID, err := Resolve(root)
		if err != nil {
			return false, err
		}
		if rootID.Contains(target) {
			return true, nil
		}
	}
	return false, nil
}
