package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. Junctions need no special
// privilege and are traversed by the filesystem exactly like symlinks, so they
// exercise the same escape on developer machines where symlink creation is
// denied. The test is skipped when neither is available.
func mustLinkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

func TestValidatePathRejectsPathBelowLinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	mustLinkDir(t, outside, filepath.Join(root, "leakdir"))

	// Positive control: the defense must not reject genuine in-root paths.
	inRoot := filepath.Join(root, "real.txt")
	if err := os.WriteFile(inRoot, []byte("in root"), 0o644); err != nil {
		t.Fatalf("WriteFile in-root: %v", err)
	}
	if _, err := validatePath(inRoot, []string{root}); err != nil {
		t.Fatalf("validatePath in-root file: %v", err)
	}

	tests := []struct {
		name string
		path string
	}{
		{name: "existing file below link", path: filepath.Join(root, "leakdir", "secret.txt")},
		{name: "missing file below link", path: filepath.Join(root, "leakdir", "not-created-yet.txt")},
		{name: "the link itself", path: filepath.Join(root, "leakdir")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := validatePath(tt.path, []string{root}); !errors.Is(err, ErrSandboxViolation) {
				t.Errorf("validatePath(%s) error = %v, want ErrSandboxViolation", tt.path, err)
			}
		})
	}
}

// A sandbox root may itself be a link — Windows redirects several well-known
// directories that way. Resolving it must not lock the user out of their own
// project directory.
func TestValidatePathAllowsLinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkedRoot := filepath.Join(base, "linked")
	mustLinkDir(t, real, linkedRoot)

	for _, name := range []string{"a.txt", "missing.txt"} {
		if _, err := validatePath(filepath.Join(linkedRoot, name), []string{linkedRoot}); err != nil {
			t.Errorf("validatePath(%s) through linked root: %v", name, err)
		}
	}
}

func TestIsMemoryRepoDetectsLinkedPath(t *testing.T) {
	base := t.TempDir()
	memRepo := filepath.Join(base, "memrepo")
	if err := os.MkdirAll(memRepo, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	attached := filepath.Join(base, "attached")
	mustLinkDir(t, memRepo, attached)

	// A link attached as a project directory resolves onto the memory repo, so
	// the C2 lock must still fire.
	if in, err := isMemoryRepo(attached, []string{memRepo}); err != nil || !in {
		t.Errorf("isMemoryRepo via linked path = (%v, %v), want (true, nil) — C2 bypass", in, err)
	}
	if in, err := isMemoryRepo(filepath.Join(attached, "sub"), []string{memRepo}); err != nil || !in {
		t.Errorf("isMemoryRepo below linked path = (%v, %v), want (true, nil) — C2 bypass", in, err)
	}
	unrelated := filepath.Join(base, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if in, err := isMemoryRepo(unrelated, []string{memRepo}); err != nil || in {
		t.Errorf("isMemoryRepo unrelated dir = (%v, %v), want (false, nil)", in, err)
	}
}

// An unresolvable path must reject the call. Continuing to the next root, or
// falling back to a lexical comparison, would let a path whose physical
// location is unknown be judged as though it were known.
func TestValidatePathRejectsUnresolvablePath(t *testing.T) {
	root := t.TempDir()

	if _, err := validatePath(filepath.Join(root, "bad\x00name"), []string{root}); err == nil {
		t.Error("validatePath accepted a path it could not resolve")
	}
	if _, err := validatePath(filepath.Join(root, "ok.txt"), []string{filepath.Join(root, "bad\x00root")}); err == nil {
		t.Error("validatePath accepted a call whose sandbox root could not be resolved")
	}
}

func TestValidatePathRejectsSiblingWithSharedPrefix(t *testing.T) {
	root := t.TempDir()
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("MkdirAll sibling: %v", err)
	}
	outside := filepath.Join(sibling, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	if _, err := validatePath(outside, []string{root}); !errors.Is(err, ErrSandboxViolation) {
		t.Fatalf("validatePath sibling error = %v, want ErrSandboxViolation", err)
	}
}

func TestValidatePathWindowsCaseInsensitiveMissingPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows path casing")
	}
	root := t.TempDir()
	missing := filepath.Join(root, "missing.txt")

	got, err := validatePath(missing, []string{strings.ToUpper(root)})
	if err != nil {
		t.Fatalf("validatePath mixed-case root: %v", err)
	}
	if got != missing {
		t.Fatalf("validatePath = %q, want %q", got, missing)
	}
}
