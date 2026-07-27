//go:build windows

package pathid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows paths are case-insensitive, so two spellings that differ only in case
// name one place. A case-sensitive identity would hand a second git lock to the
// same repository and let a sandbox root reject its own contents.
func TestResolveIgnoresCaseOnWindows(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "MixedCase")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "File.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, name := range []string{"File.txt", "missing.txt"} {
		same, err := Same(filepath.Join(dir, name), filepath.Join(strings.ToUpper(dir), strings.ToLower(name)))
		if err != nil {
			t.Fatalf("Same(%s): %v", name, err)
		}
		if !same {
			t.Errorf("%s compared unequal to its own upper-cased spelling", name)
		}
	}

	upper, err := Resolve(strings.ToUpper(dir))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Both sides have to be resolved. newID only lower-cases; it does not
	// canonicalize, so on a runner whose TEMP contains an 8.3 component
	// (GitHub's Windows image gives RUNNER~1) the unresolved child keeps the
	// short spelling while the resolved root carries the long one, and
	// containment correctly reports two different places.
	child, err := Resolve(filepath.Join(dir, "File.txt"))
	if err != nil {
		t.Fatalf("Resolve child: %v", err)
	}
	if !upper.Contains(child) {
		t.Error("an upper-cased root does not contain its own file")
	}
}

// An 8.3 short name is another spelling of the same file. GetFinalPathNameByHandle
// normalizes it away; filepath.EvalSymlinks does not, which is one of the
// reasons this package does not use it.
//
// 8.3 generation is disabled on many volumes, so the test skips when the alias
// cannot be produced rather than asserting the feature is on.
func TestResolveNormalizesShortNameOnWindows(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, "a-directory-with-a-long-name")
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(long, "contents.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	short := shortPathName(t, long)
	if strings.EqualFold(short, long) {
		t.Skip("8.3 short names are not generated on this volume")
	}

	same, err := Same(long, short)
	if err != nil {
		t.Fatalf("Same: %v", err)
	}
	if !same {
		t.Errorf("short name %q and long name %q resolved to different identities", short, long)
	}

	// The alias must also not let a path escape a root spelled the long way.
	root, err := Resolve(long)
	if err != nil {
		t.Fatalf("Resolve long: %v", err)
	}
	viaShort, err := Resolve(filepath.Join(short, "contents.txt"))
	if err != nil {
		t.Fatalf("Resolve via short: %v", err)
	}
	if !root.Contains(viaShort) {
		t.Errorf("%q is not contained by %q when reached through its short name", viaShort, root)
	}
}

// shortPathName returns the 8.3 alias for path, or path itself when the volume
// does not generate one.
func shortPathName(t *testing.T, path string) string {
	t.Helper()
	in, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(in, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Skipf("GetShortPathName unavailable: %v", err)
	}
	if n > uint32(len(buf)) {
		buf = make([]uint16, n+1)
		if _, err := windows.GetShortPathName(in, &buf[0], uint32(len(buf))); err != nil {
			t.Skipf("GetShortPathName unavailable: %v", err)
		}
	}
	return windows.UTF16ToString(buf)
}
