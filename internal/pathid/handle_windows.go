//go:build windows

package pathid

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// CanonicalFile returns the physical path of an already-open file.
//
// It exists so a caller can validate the exact handle it is about to read from,
// rather than canonicalizing a path and reopening it by name afterwards. A
// path-based check and a later open are two different operations on two
// different resolutions: whatever the name referred to during the check can be
// replaced before the open.
//
// GetFinalPathNameByHandle answers for the handle itself, so the file that was
// checked and the file that is read are the same object by construction.
func CanonicalFile(f *os.File) (string, error) {
	if f == nil {
		return "", fmt.Errorf("pathid: nil file")
	}
	h := windows.Handle(f.Fd())
	const flags = fileNameNormalized | volumeNameDOS
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
	if err != nil {
		return "", fmt.Errorf("pathid: canonical path for %s: %w", f.Name(), err)
	}
	if n > uint32(len(buf)) {
		buf = make([]uint16, n+1)
		if _, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags); err != nil {
			return "", fmt.Errorf("pathid: canonical path for %s: %w", f.Name(), err)
		}
	}
	return stripExtendedPrefix(windows.UTF16ToString(buf)), nil
}
