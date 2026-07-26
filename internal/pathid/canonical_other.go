//go:build !windows

package pathid

import "path/filepath"

// Canonical returns the physical path of an existing file or directory.
// On non-Windows systems symlinks are the only reparse mechanism and
// filepath.EvalSymlinks resolves them completely, so it is the whole
// implementation. See canonical_windows.go for why Windows needs more.
//
// The path must exist: callers resolving a path that may not exist yet use
// Resolve, which walks up to the deepest existing component.
func Canonical(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
