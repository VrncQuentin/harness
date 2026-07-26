//go:build !windows

package tools

import "path/filepath"

// canonicalPath returns the physical path of an existing file or directory.
// On non-Windows systems symlinks are the only reparse mechanism and
// filepath.EvalSymlinks resolves them completely, so it is the whole
// implementation. See canonical_windows.go for why Windows needs more.
func canonicalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
