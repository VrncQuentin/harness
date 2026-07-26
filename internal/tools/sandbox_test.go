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

func TestCanonicalPathHasNoExtendedPrefix(t *testing.T) {
	dir := t.TempDir()
	got, err := canonicalPath(dir)
	if err != nil {
		t.Fatalf("canonicalPath: %v", err)
	}
	// A \\?\-prefixed result would never compare equal to a user-configured
	// sandbox root, silently rejecting every path.
	if strings.HasPrefix(got, `\\?\`) {
		t.Errorf("canonicalPath = %q, still carries the extended-length prefix", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("canonicalPath = %q, want an absolute path", got)
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
	if !isMemoryRepo(attached, []string{memRepo}) {
		t.Error("isMemoryRepo via linked path = false, want true (C2 bypass)")
	}
	if !isMemoryRepo(filepath.Join(attached, "sub"), []string{memRepo}) {
		t.Error("isMemoryRepo below linked path = false, want true (C2 bypass)")
	}
	unrelated := filepath.Join(base, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if isMemoryRepo(unrelated, []string{memRepo}) {
		t.Error("isMemoryRepo unrelated dir = true, want false")
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
