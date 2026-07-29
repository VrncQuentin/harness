package rootfs

import (
	"bytes"
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
	return a
}

func mustOpen(t *testing.T, a *Anchor) *Root {
	t.Helper()
	r, err := a.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

func writeRootFile(t *testing.T, r *Root, name, content string) {
	t.Helper()
	if err := r.WriteStreamAtomic(name, bytes.NewReader([]byte(content)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRootFile(t *testing.T, r *Root, name string) string {
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
	writeRootFile(t, r, "hello.txt", "world")
	r.Close()

	r2 := mustOpen(t, a)
	got := readRootFile(t, r2, "hello.txt")
	r2.Close()
	if got != "world" {
		t.Errorf("expected world, got %s", got)
	}
}

func TestAnchor_ConstructionThroughSymlinkSucceeds(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, dir, link)

	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	writeRootFile(t, r, "f", "ok")
	r.Close()

	r2 := mustOpen(t, a)
	got := readRootFile(t, r2, "f")
	r2.Close()
	if got != "ok" {
		t.Error("stable symlink should bind physical target")
	}
}

func TestAnchor_ConstructionThroughJunctionSucceeds(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	junctionOrSkip(t, dir, link)

	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	writeRootFile(t, r, "f", "ok")
	r.Close()

	r2 := mustOpen(t, a)
	got := readRootFile(t, r2, "f")
	r2.Close()
	if got != "ok" {
		t.Error("stable junction should bind physical target")
	}
}

func TestAnchor_SameNameReplacementFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	r := mustOpen(t, a)
	r.Close()

	// Replace the directory with a completely different one.
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
		t.Error("replacement under same pathname should fail identity check")
	}
}

func TestAnchor_OriginalRenamedAsideFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	r := mustOpen(t, a)
	r.Close()

	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := a.Open()
	if err == nil {
		t.Error("original renamed aside should fail identity check")
	}
}

func TestAnchor_RePointedAliasFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, dir1, link)

	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	r.Close()

	// Re-point the symlink to a different directory.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, dir2, link)

	_, err := a.Open()
	if err == nil {
		t.Error("re-pointed symlink alias should fail identity check")
	}
}

func TestAnchor_RePointedJunctionFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	junctionOrSkip(t, dir1, link)

	a := mustNewAnchor(t, link)
	r := mustOpen(t, a)
	r.Close()

	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	junctionOrSkip(t, dir2, link)

	_, err := a.Open()
	if err == nil {
		t.Error("re-pointed junction alias should fail identity check")
	}
}

func TestAnchor_DoesNotExist(t *testing.T) {
	_, err := NewAnchor(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Error("nonexistent path should fail construction")
	}
}

func TestAnchor_PathReturnsConfiguredName(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)
	if a.Path() != dir {
		t.Errorf("Path() = %s, want %s", a.Path(), dir)
	}
}

func TestAnchor_WritesThroughReopenedRootAreVisible(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	r1 := mustOpen(t, a)
	writeRootFile(t, r1, "a", "1")
	r1.Close()

	r2 := mustOpen(t, a)
	writeRootFile(t, r2, "b", "2")
	r2.Close()

	r3 := mustOpen(t, a)
	if readRootFile(t, r3, "a") != "1" || readRootFile(t, r3, "b") != "2" {
		t.Error("writes through separate reopens should be visible")
	}
	r3.Close()
}

func TestAnchor_DoesNotHoldHandleBetweenOperations(t *testing.T) {
	dir := t.TempDir()
	a := mustNewAnchor(t, dir)

	r := mustOpen(t, a)
	r.Close()

	// The directory should be renameable since no handle is held.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal("directory should be renameable when no handle is held:", err)
	}
}
