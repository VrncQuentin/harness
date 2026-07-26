// Package pathid establishes the physical identity of a filesystem path.
//
// Two questions in this repository need the same answer and must not answer it
// differently: "is this path inside that root" (the tool sandbox and the C2
// memory-repo lock) and "are these two paths the same repository" (the git
// write lock). Both are security or correctness boundaries, and both were
// previously built on filepath.EvalSymlinks, which cannot express identity on
// Windows — see Canonical.
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

// Resolve returns the physical path of path, canonicalizing the deepest
// component that exists and re-appending the components below it.
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
func Resolve(path string) (string, error) {
	return resolveWith(Canonical, path)
}

// resolveWith is Resolve with an injectable canonicalizer, so the error-kind
// policy can be tested without depending on platform-specific ways to make a
// path unreadable.
func resolveWith(canonical func(string) (string, error), path string) (string, error) {
	current := filepath.Clean(path)
	var tail []string
	for {
		resolved, err := canonical(current)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("pathid: resolve %s: %w", path, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("pathid: resolve %s: %w", path, err)
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

// WithinRoot reports whether path is root or lies below it. Both arguments
// must already be canonical: comparing a raw path against a resolved one is
// how a link escapes.
func WithinRoot(path, root string) bool {
	path = Key(path)
	root = Key(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Key returns a comparison key for an already-canonical path. Windows paths
// are case-insensitive, so identity there ignores case.
func Key(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// SameOrWithin reports whether path is inside any of roots, resolving both
// sides. A resolution failure on either side is returned rather than treated
// as "not inside".
func SameOrWithin(path string, roots []string) (bool, error) {
	resolvedPath, err := Resolve(path)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		resolvedRoot, err := Resolve(root)
		if err != nil {
			return false, err
		}
		if WithinRoot(resolvedPath, resolvedRoot) {
			return true, nil
		}
	}
	return false, nil
}
