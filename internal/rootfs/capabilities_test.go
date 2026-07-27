package rootfs

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// linkDir creates a directory link, preferring a symlink and falling back to a
// Windows junction, which needs no privilege and is traversed the same way.
func linkDir(t *testing.T, target, link string) {
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

func openRoot(t *testing.T, dir string) *Root {
	t.Helper()
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// A walk that inspects a child's name and then resolves the name again to
// descend can be sent somewhere else in between. Descending through a pinned
// handle means what was inspected is what is entered — and in particular a
// directory swapped in for one that leads outside cannot make the walk report
// entries from outside the root.
func TestRoot_WalkDoesNotFollowALinkOutOfTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "bait.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkDir(t, outside, filepath.Join(root, "linked"))

	var seen []string
	err := openRoot(t, root).Walk("", func(e WalkEntry) (bool, error) {
		seen = append(seen, filepath.ToSlash(e.Rel))
		return false, nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, rel := range seen {
		if strings.Contains(rel, "bait") {
			t.Fatalf("Walk descended through the link and reported %q", rel)
		}
	}
	if !slicesContains(seen, "inside.txt") {
		t.Fatalf("Walk missed a real entry: %v", seen)
	}
}

// The pinned-descent property, staged where it is actually observable.
//
// A link *leaf* proves nothing here: Lstat reports it as a link and the walk
// declines to descend either way, so a traversal that re-resolved names from
// the top would pass that test unchanged. What separates the two designs is
// what happens when the name the walk started from stops meaning the same
// directory partway through. Re-pointing it between one level and the next
// sends a name-rejoining descent into the replacement; a descent through pinned
// child handles stays in the directory it is already holding.
func TestRoot_WalkKeepsDescendingInsideThePinnedTreeAfterTheRootIsRepointed(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(evil, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// "aaa.txt" sorts before "sub", so the walk visits it first and the
	// re-point below lands between the enumeration and the descent.
	if err := os.WriteFile(filepath.Join(real, "aaa.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "sub", "inside.txt"), []byte("i"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evil, "sub", "secret.txt"), []byte("s"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	name := filepath.Join(base, "link")
	linkDir(t, real, name)
	r := openRoot(t, name)

	repointed := false
	var seen []string
	err := r.Walk("", func(e WalkEntry) (bool, error) {
		seen = append(seen, filepath.ToSlash(e.Rel))
		if e.Name == "sub" && !repointed {
			repointed = true
			if err := os.Remove(name); err != nil {
				t.Skipf("cannot re-point the walked root here: %v", err)
			}
			linkDir(t, evil, name)
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !repointed {
		t.Fatal("the re-point never happened; the window was not exercised")
	}
	if slicesContains(seen, "sub/secret.txt") {
		t.Fatalf("the walk descended into the replacement directory: %v", seen)
	}
	if !slicesContains(seen, "sub/inside.txt") {
		t.Fatalf("the walk lost the pinned subtree: %v", seen)
	}
}

// AppendSync resolves its target through the root, so an append addressed
// through a directory link that leaves the root is refused rather than
// extending a file outside it.
func TestRoot_AppendSyncDoesNotFollowALinkOutOfTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	const bait = "OUTSIDE"
	if err := os.WriteFile(filepath.Join(outside, "log"), []byte(bait), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkDir(t, outside, filepath.Join(root, "linked"))

	if err := openRoot(t, root).AppendSync("linked/log", []byte("appended"), 0o644); err == nil {
		t.Error("an append through a link out of the root was accepted")
	}
	got, err := os.ReadFile(filepath.Join(outside, "log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != bait {
		t.Fatalf("the append reached a file outside the root: %q", got)
	}
}

// Walk reports a symbolic link as a link rather than descending into it, which
// is what filepath.WalkDir did and what the memory repo's Walk contract
// promises. A junction stands in where symlinks need a privilege.
func TestRoot_WalkReportsLinkedDirectoryWithoutDescending(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	inner := filepath.Join(root, "real")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inner, "deep.txt"), []byte("z"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkDir(t, inner, filepath.Join(root, "alias"))

	var seen []string
	if err := openRoot(t, root).Walk("", func(e WalkEntry) (bool, error) {
		seen = append(seen, filepath.ToSlash(e.Rel))
		return false, nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if slicesContains(seen, "alias/deep.txt") {
		t.Fatalf("Walk descended into a linked directory: %v", seen)
	}
	if !slicesContains(seen, "real/deep.txt") {
		t.Fatalf("Walk missed the real subtree: %v", seen)
	}
}

// Returning skip for a directory prunes it and everything below.
func TestRoot_WalkSkipPrunesSubtree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.md"), []byte("k"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var seen []string
	if err := openRoot(t, root).Walk("", func(e WalkEntry) (bool, error) {
		if e.Name == ".git" {
			return true, nil
		}
		seen = append(seen, filepath.ToSlash(e.Rel))
		return false, nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, rel := range seen {
		if strings.HasPrefix(rel, ".git") {
			t.Fatalf("skip did not prune the subtree: %v", seen)
		}
	}
	if !slicesContains(seen, "keep.md") {
		t.Fatalf("Walk missed keep.md: %v", seen)
	}
}

// A link cycle inside the root is followed by os.Root — its guarantee is
// containment, not the absence of links — so the walk needs its own bound or it
// never returns.
func TestRoot_WalkBoundsADirectoryCycle(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "a")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// a/loop points back at the root, so the tree is infinitely deep.
	linkDir(t, root, filepath.Join(inner, "loop"))

	err := openRoot(t, root).Walk("", func(WalkEntry) (bool, error) { return false, nil })
	if err == nil {
		// A junction stores an absolute target, which os.Root refuses to
		// traverse, so on Windows the cycle is simply not entered. Either
		// outcome is a terminating walk, which is the property under test.
		return
	}
	if !strings.Contains(err.Error(), "nests deeper") && !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("Walk failed for an unexpected reason: %v", err)
	}
}

// AppendSync never truncates, whatever else is going on.
func TestRoot_AppendSyncPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	r := openRoot(t, dir)
	if err := r.AppendSync("log", []byte("one\n"), 0o644); err != nil {
		t.Fatalf("AppendSync: %v", err)
	}
	if err := r.AppendSync("log", []byte("two\n"), 0o644); err != nil {
		t.Fatalf("AppendSync: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "one\ntwo\n" {
		t.Fatalf("content = %q, want %q", got, "one\ntwo\n")
	}
}

// OpenReadWrite must not truncate: the index measures the file before it
// appends, and a truncating open would make that measurement zero.
func TestRoot_OpenReadWriteDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vectors.bin"), []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := openRoot(t, dir).OpenReadWrite("vectors.bin", 0o644)
	if err != nil {
		t.Fatalf("OpenReadWrite: %v", err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if size != 10 {
		t.Fatalf("Size = %d, want 10 — the open truncated the file", size)
	}
}

// The measure/append/roll-back sequence has to hold on one handle: appending
// then truncating back must leave exactly what was there before.
func TestRoot_AppendThenTruncateRollsBackThroughOneHandle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vectors.bin"), []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := openRoot(t, dir).OpenReadWrite("vectors.bin", 0o644)
	if err != nil {
		t.Fatalf("OpenReadWrite: %v", err)
	}
	defer func() { _ = f.Close() }()

	start, err := f.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if err := f.Append([]byte("appended-and-rolled-back")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := f.Truncate(start); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "vectors.bin"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "keep" {
		t.Fatalf("rollback left %q, want %q", got, "keep")
	}
}

// A failed atomic write must not leave a temporary file behind, and must not
// remove the file that was already there.
func TestRoot_WriteStreamAtomicFailureLeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("previous"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := openRoot(t, dir)
	err := r.WriteStreamAtomic("manifest.json", errReader{}, 0o644)
	if err == nil {
		t.Fatal("expected the copy failure to be returned")
	}
	got, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "previous" {
		t.Fatalf("a failed write disturbed the existing file: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempNamePrefix) {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// Clone hands out an independent handle on the same directory: closing one must
// not disturb the other.
func TestRoot_CloneIsIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := openRoot(t, dir)
	clone, err := r.Clone()
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := clone.Close(); err != nil {
		t.Fatalf("Close clone: %v", err)
	}
	if _, err := r.ReadFile("f.txt"); err != nil {
		t.Fatalf("closing the clone disturbed the original: %v", err)
	}
}

// CreateExclusive claims a name atomically rather than replacing whatever holds
// it, which is what makes scaffolding safe to run against a repo somebody else
// is also writing.
func TestRoot_CreateExclusiveRefusesAnExistingName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitkeep"), []byte("user data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := openRoot(t, dir).CreateExclusive(".gitkeep", nil, 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want fs.ErrExist", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".gitkeep"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "user data" {
		t.Fatalf("CreateExclusive overwrote an existing file: %q", got)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
