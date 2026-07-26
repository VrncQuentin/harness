//go:build windows

package pathid

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandle flags. Both are zero and x/sys/windows does not
// export them, so they are named here for the call site to stay readable.
const (
	fileNameNormalized = 0x0 // canonical component names, not 8.3 short names
	volumeNameDOS      = 0x0 // drive-letter form, not a \\?\Volume{GUID} path
)

// Canonical returns the physical path of an existing file or directory,
// resolving symlinks, junctions, mount points, and 8.3 short names.
//
// It stands in for filepath.EvalSymlinks, which cannot be trusted for
// containment checks on Windows. EvalSymlinks only follows entries carrying
// ModeSymlink; a junction is reported irregular, so EvalSymlinks returns the
// junction path unchanged, and a containment comparison built on it accepts a
// path that physically reads outside the sandbox root. Worse, EvalSymlinks
// fails outright on any path below a junction even where os.ReadFile succeeds,
// which turns the escape into a silent one.
//
// The path must exist: a handle is required. Callers resolve the deepest
// existing ancestor for paths that are about to be created.
func Canonical(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only handle used solely for resolution

	h := windows.Handle(f.Fd())
	const flags = fileNameNormalized | volumeNameDOS
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
	if err != nil {
		return "", fmt.Errorf("pathid: canonical path %s: %w", path, err)
	}
	if n > uint32(len(buf)) {
		// n is the required length in UTF-16 words, excluding the terminator.
		buf = make([]uint16, n+1)
		if _, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags); err != nil {
			return "", fmt.Errorf("pathid: canonical path %s: %w", path, err)
		}
	}
	return stripExtendedPrefix(windows.UTF16ToString(buf)), nil
}

// stripExtendedPrefix removes the \\?\ extended-length prefix that
// GetFinalPathNameByHandle always returns, so results compare directly against
// the ordinary paths a user configures as sandbox roots. A UNC result is
// returned in its familiar \\server\share form.
func stripExtendedPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}
