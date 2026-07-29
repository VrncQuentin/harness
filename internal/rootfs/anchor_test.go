package rootfs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink requires Developer Mode on Windows")
		}
		t.Fatal(err)
	}
}

func junctionOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("junction is Windows-only")
	}
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %s -> %s failed: %v\n%s", link, target, err, string(out))
	}
}

func mustNewAnchor(t *testing.T, path string) *Anchor {
	t.Helper()
	a, err := NewAnchor(path)
	if err != nil {
		t.Fatalf("NewAnchor(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func mustOpen(t *testing.T, a *Anchor) *Root {
	t.Helper()
	r, err := a.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func writeRoot(t *testing.T, r *Root, name, content string) {
	t.Helper()
	if err := r.WriteStreamAtomic(name, bytes.NewReader([]byte(content)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRoot(t *testing.T, r *Root, name string) string {
	t.Helper()
	data, err := r.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// ---------- tests ----------

func TestAnchor_NormalReopen(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)
	r := mustOpen(t, a)
	writeRoot(t, r, "hello.txt", "world")
	_ = r.Close()

	r2 := mustOpen(t, a)
	if got := readRoot(t, r2, "hello.txt"); got != "world" {
		t.Errorf("expected world, got %s", got)
	}
	_ = r2.Close()
}

func TestAnchor_ConstructionThroughSymlinkSucceeds(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, dir, link)
	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	writeRoot(t, r, "f", "ok")
	_ = r.Close()

	r2 := mustOpen(t, a)
	if got := readRoot(t, r2, "f"); got != "ok" {
		t.Error("stable symlink should bind physical target")
	}
	_ = r2.Close()
}

func TestAnchor_ConstructionThroughJunctionSucceeds(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	junctionOrSkip(t, dir, link)
	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	writeRoot(t, r, "f", "ok")
	_ = r.Close()

	r2 := mustOpen(t, a)
	if got := readRoot(t, r2, "f"); got != "ok" {
		t.Error("stable junction should bind physical target")
	}
	_ = r2.Close()
}

func TestAnchor_SameNameReplacementFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	// Try to remove the pinned directory.  If the handle blocks
	// removal, close and retry to prove the handle was the cause.
	// If removal succeeds, install a replacement and assert the
	// anchor rejects it.
	if err := os.RemoveAll(dir); err != nil {
		_ = a.Close()
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal("removal should succeed after anchor closed:", err)
		}
		return
	}

	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.MkdirAll(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dir); err != nil {
		t.Fatal(err)
	}
	_, err := a.Open()
	if err == nil {
		t.Error("anchor should reject replacement under same pathname")
	}
}

func TestAnchor_RenameAsideFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	moved := filepath.Join(t.TempDir(), "moved-aside")
	if err := os.Rename(dir, moved); err != nil {
		_ = a.Close()
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal("rename should succeed after anchor closed:", err)
		}
		return
	}

	// The original was renamed aside.  Create a replacement at the
	// configured name and assert the anchor rejects it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := a.Open()
	if err == nil {
		t.Error("anchor should reject replacement after original renamed aside")
	}
}

func TestAnchor_RePointedAliasFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, dir1, link)

	a := mustNewAnchor(t, link)

	if err := os.Remove(link); err != nil {
		_ = a.Close()
		if err := os.Remove(link); err != nil {
			t.Fatal("symlink removal should succeed after anchor closed:", err)
		}
		return
	}
	symlinkOrSkip(t, dir2, link)
	_, err := a.Open()
	if err == nil {
		t.Error("anchor should reject re-pointed symlink alias")
	}
}

func TestAnchor_RePointedJunctionFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	junctionOrSkip(t, dir1, link)

	a := mustNewAnchor(t, link)

	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	junctionOrSkip(t, dir2, link)

	_, err := a.Open()
	if err == nil {
		t.Error("anchor should reject re-pointed junction alias")
	}
}

func TestAnchor_DoesNotExist(t *testing.T) {
	_, err := NewAnchor(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Error("nonexistent path should fail construction")
	}
}

func TestAnchor_WindowsIdentity(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}
	dir := t.TempDir()
	upper := filepath.Join(dir, "UPPER")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(dir, "upper")
	a := mustNewAnchor(t, lower)
	r := mustOpen(t, a)
	writeRoot(t, r, "f", "ok")
	_ = r.Close()

	r2 := mustOpen(t, a)
	if got := readRoot(t, r2, "f"); got != "ok" {
		t.Error("case-insensitive path should bind same directory on Windows")
	}
	_ = r2.Close()
}

func TestAnchor_IdentityFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)
	sentinel := errors.New("injected stat failure")

	// Fail on the first stat call (reopened root).
	var calls int
	_, err := a.open(func(r *Root) (fs.FileInfo, error) {
		calls++
		if calls == 1 {
			return nil, sentinel
		}
		return r.root.Stat(".")
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("reopened-stat failure should propagate, got %v", err)
	}

	// Fail on the second stat call (pinned root).
	calls = 0
	_, err = a.open(func(r *Root) (fs.FileInfo, error) {
		calls++
		if calls == 2 {
			return nil, sentinel
		}
		return r.root.Stat(".")
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("pinned-stat failure should propagate, got %v", err)
	}

	// Anchor still works normally after injected failures.
	r := mustOpen(t, a)
	_ = r.Close()
}

func TestAnchor_ExplicitCloseReleasesHandle(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAnchor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal("Close should succeed:", err)
	}

	_, err = a.Open()
	if err == nil {
		t.Error("Open should fail after Close")
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Error("directory removal should succeed after anchor handle closed:", err)
	}
}
