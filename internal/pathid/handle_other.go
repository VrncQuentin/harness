//go:build !windows

package pathid

import (
	"fmt"
	"os"
	"strings"
)

// CanonicalFile returns the physical path of an already-open file.
//
// It exists so a caller can validate the exact handle it is about to read from,
// rather than canonicalizing a path and reopening it by name afterwards. A
// path-based check and a later open are two different operations on two
// different resolutions: whatever the name referred to during the check can be
// replaced before the open.
//
// /proc/self/fd/<n> names the open description itself, so the file that was
// checked and the file that is read are the same object by construction. The
// harness targets Linux and Windows only, which is what makes this available.
func CanonicalFile(f *os.File) (string, error) {
	if f == nil {
		return "", fmt.Errorf("pathid: nil file")
	}
	resolved, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return "", fmt.Errorf("pathid: canonical path for %s: %w", f.Name(), err)
	}
	// A file unlinked after opening keeps its handle valid and is reported with
	// this suffix. The path it names is gone, so containment cannot be judged.
	if strings.HasSuffix(resolved, " (deleted)") {
		return "", fmt.Errorf("pathid: %s was removed while open", f.Name())
	}
	return resolved, nil
}
