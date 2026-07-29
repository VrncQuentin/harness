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
	r := mustOpen(t, a)
	_ = r.Close()

	if runtime.GOOS == "windows" {
		if err := os.RemoveAll(dir); err == nil {
			t.Error("anchor handle should block directory removal on Windows")
		}
		return
	}

	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.MkdirAll(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
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

func TestAnchor_RePointedAliasFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, dir1, link)

	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	_ = r.Close()

	if err := os.Remove(link); err != nil {
		// On Windows the anchor handle may block the removal.
		// If it does, that is its own defence.
		if runtime.GOOS == "windows" {
			return
		}
		t.Fatal("unexpected symlink removal failure:", err)
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
	r := mustOpen(t, a)
	_ = r.Close()

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
	failStat := func(r *Root) (fs.FileInfo, error) { return nil, sentinel }

	// Injured reopened stat.
	_, err := a.open(failStat, nil)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("reopened-stat failure should propagate, got %v", err)
	}

	// Injured pinned stat.
	_, err = a.open(nil, failStat)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("pinned-stat failure should propagate, got %v", err)
	}

	// Anchor still works after injected failures.
	r := mustOpen(t, a)
	_ = r.Close()
}

func TestAnchor_WritesThroughReopenedRootAreVisible(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	r1 := mustOpen(t, a)
	writeRoot(t, r1, "a", "1")
	_ = r1.Close()

	r2 := mustOpen(t, a)
	writeRoot(t, r2, "b", "2")
	_ = r2.Close()

	r3 := mustOpen(t, a)
	if readRoot(t, r3, "a") != "1" || readRoot(t, r3, "b") != "2" {
		t.Error("writes through separate reopens should be visible")
	}
	_ = r3.Close()
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

	// Open must fail after Close.
	_, err = a.Open()
	if err == nil {
		t.Error("Open should fail after Close")
	}

	// On Windows, the directory removal that was blocked by the
	// live handle must now succeed.
	if runtime.GOOS == "windows" {
		if err := os.RemoveAll(dir); err != nil {
			t.Error("directory removal should succeed after anchor handle closed:", err)
		}
	} else {
		if err := os.RemoveAll(dir); err != nil {
			t.Error("directory removal should succeed:", err)
		}
	}
}
