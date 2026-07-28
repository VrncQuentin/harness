package rootfs

import (
	"bytes"
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

// A directory entry's type from the initial listing only says what it was at
// listing time. os.Root follows an in-root symlink rather than refusing it, so
// a name entered as a real directory in the listing but replaced by an
// in-root symlink before the walk reaches it would be opened — and, without a
// fresh check, entered — on the strength of stale metadata alone.
//
// This stages exactly that: the visitor for an entry that sorts first
// ("aaa") removes a directory that sorts later ("zdir") and replaces it with
// a symlink to a third, nested directory holding bait content, before the
// walk ever reaches "zdir". The walk must report "zdir" as the link it now is
// and must not read the bait content through it.
func TestRoot_WalkRefusesADirectoryReplacedByAnInRootSymlinkAfterListing(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	// Nested one level down so it is not itself a sibling the walk would
	// separately enumerate at the top level.
	target := filepath.Join(root, "aaa", "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const secret = "SECRET-REACHED-BY-A-POST-LISTING-SYMLINK"
	if err := os.WriteFile(filepath.Join(target, "bait.txt"), []byte(secret), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	zdir := filepath.Join(root, "zdir")
	if err := os.MkdirAll(zdir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(zdir, "real.txt"), []byte("the genuine zdir"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := openRoot(t, root)
	swapped := false
	var sawZdirAsLink bool
	var disclosedSecret bool
	err := r.Walk("", func(e WalkEntry) (bool, error) {
		if e.Name == "aaa" && !swapped {
			swapped = true
			if err := os.RemoveAll(zdir); err != nil {
				t.Fatalf("RemoveAll zdir: %v", err)
			}
			// The target must be relative, not absolute: os.Root refuses an
			// absolute link target unconditionally — the same rule that
			// refuses a Windows junction, which always stores one — so an
			// absolute-target symlink here would be rejected regardless of
			// whether this fix exists, proving nothing about the property
			// under test. A relative target, resolved against the symlink's
			// own parent directory ("root"), is what os.Root actually follows
			// when it stays in-root.
			if err := os.Symlink(filepath.Join("aaa", "elsewhere"), zdir); err != nil {
				t.Skipf("file symlinks unavailable in this environment: %v", err)
			}
		}
		if e.Name == "zdir" {
			if e.Info.Mode()&fs.ModeSymlink != 0 {
				sawZdirAsLink = true
			}
		}
		if e.Name == "bait.txt" && filepath.Base(filepath.Dir(e.Rel)) == "zdir" {
			// Reached "bait.txt" by descending through "zdir" as though it
			// were still the real directory — the walk followed the
			// replacement symlink into its target.
			disclosedSecret = true
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !swapped {
		t.Fatal("the swap never ran; the window was not exercised")
	}
	if disclosedSecret {
		t.Fatal("the walk descended through a directory that had become a symlink after listing, disclosing the target's content")
	}
	if !sawZdirAsLink {
		t.Fatal("zdir was not reported as a link even though it had become one before the walk reached it")
	}
}

// The previous test closes the case where a symlink replacing a directory is
// still in place when walkDir's Lstat runs. This one closes a narrower case
// the old "is the Lstat result a symlink" check could not catch at all: an
// ordinary, non-symlink directory replacing another ordinary directory, in
// the window between OpenChild (which has already pinned the original) and
// the Lstat that is supposed to describe what the name currently holds.
// Neither the original nor the replacement is a link, so a check that only
// asks "is this a symlink" cannot tell them apart — it sees a directory
// either way and proceeds. Only comparing child (what OpenChild actually
// pinned) against parentInfo (what the name names now) as filesystem
// objects, via os.SameFile, tells them apart. No symlink privilege is needed
// to exercise this: the new walkDirHooked seam fires right after OpenChild
// has pinned the original "sub", before the Lstat that is supposed to
// re-describe it runs, and the hook renames "sub" aside and recreates it as a
// plain, unrelated directory in that window.
func TestRoot_WalkRequiresChildAndParentLstatToDescribeTheSameObject(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const secret = "SECRET-INSIDE-THE-DIRECTORY-OPENCHILD-PINNED"
	if err := os.WriteFile(filepath.Join(sub, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	movedAside := filepath.Join(root, "moved-aside")
	r := openRoot(t, root)
	var swapped bool
	var disclosedSecret bool

	afterOpenChild := func(name string) {
		if name != "sub" || swapped {
			return
		}
		swapped = true
		// child is already pinned to the original "sub" by the OpenChild call
		// that just returned, by descriptor rather than by path — renaming
		// the directory aside does not disturb it or the secret file inside,
		// only the name it is reachable under. "sub" is then free to be
		// recreated as a brand new, unrelated directory.
		if err := os.Rename(sub, movedAside); err != nil {
			t.Fatalf("Rename sub aside: %v", err)
		}
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("MkdirAll sub (replacement): %v", err)
		}
		if err := os.WriteFile(filepath.Join(sub, "replacement.txt"), []byte("the replacement sub"), 0o644); err != nil {
			t.Fatalf("WriteFile replacement.txt: %v", err)
		}
	}

	err := r.walkHooked("", func(e WalkEntry) (bool, error) {
		if e.Name == "secret.txt" {
			disclosedSecret = true
		}
		return false, nil
	}, afterOpenChild)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !swapped {
		t.Fatal("the swap never ran; the window was not exercised")
	}
	if disclosedSecret {
		t.Fatal("the walk descended into the directory OpenChild had pinned even though \"sub\" had since been replaced by an unrelated directory")
	}
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

// OpenRead must not truncate: a reader has no business shortening what it
// reads, and nothing about opening for read implies O_TRUNC — but the type
// exists precisely so that guarantee cannot be violated by a caller passing
// the wrong flag, because there is no flag to pass.
func TestRoot_OpenReadDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vectors.bin"), []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := openRoot(t, dir).OpenRead("vectors.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
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

// WriteStreamAtomic pins rel's destination directory once and performs the
// temp create, the failure-path remove, and the final rename all against that
// one handle. Re-resolving the directory component on each of those calls —
// the shape this replaced — would let a directory swapped in between them
// redirect the operation.
//
// The swap has to hit the destination *subdirectory itself* ("sub") to
// discriminate the two implementations, not the root's own outer name: a
// pinned os.Root handle already survives its own name changing (proven
// separately by TestRoot_WalkKeepsDescendingInsideThePinnedTreeAfterTheRootIsRepointed),
// so repointing an outer symlinked name that "real" is reached through says
// nothing about whether "sub", *inside* the already-pinned root, is resolved
// once or three times. The hook here therefore swaps "sub" itself: it fires
// right after WriteStreamAtomic pins the destination directory and before
// createTemp ever touches it, so a create/rename shape that re-resolved "sub"
// from the outer root on each call would createTemp, and then rename, against
// the replacement — landing the write there instead of in the directory that
// was actually pinned.
func TestRoot_WriteStreamAtomicPinsDestinationDirectoryOnce(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sub := filepath.Join(real, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	movedAside := filepath.Join(base, "sub-moved-aside")
	replacement := filepath.Join(base, "sub-replacement")
	if err := os.MkdirAll(replacement, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	r := openRoot(t, real)
	swapped := false
	const content = "the genuine write"
	err := r.writeStreamAtomicHooked("sub/file.txt", strings.NewReader(content), 0o644, func() {
		swapped = true
		// Take the pinned subdirectory's name out of the way and put an
		// entirely different, empty directory under the exact name that was
		// just pinned.
		if err := os.Rename(sub, movedAside); err != nil {
			t.Skipf("cannot rename the pinned subdirectory here: %v", err)
		}
		if err := os.Rename(replacement, sub); err != nil {
			t.Fatalf("rename replacement into place: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("WriteStreamAtomic: %v", err)
	}
	if !swapped {
		t.Fatal("the hook never ran; the swap window was not exercised")
	}

	if _, err := os.Stat(filepath.Join(sub, "file.txt")); err == nil {
		t.Fatal("the write landed in the replacement directory installed under the pinned name, not in the directory that was actually pinned")
	}
	got, err := os.ReadFile(filepath.Join(movedAside, "file.txt"))
	if err != nil {
		t.Fatalf("the write did not land in the directory pinned before the swap (moved aside afterward): %v", err)
	}
	if string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
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

// swapTempFileOnFirstRead stands in for a src whose Read takes real time to
// produce bytes — network I/O, a slow disk, or nothing more than an unlucky
// scheduling gap — during which anything else with access to the destination
// directory can see the temp file WriteStreamAtomic already created (its name
// is only unpredictable before that point) and replace it. It lists the
// directory itself the first time it is asked for bytes, removes whatever
// carries the tempNamePrefix, and recreates that exact name holding different
// content, before finally handing back the genuine bytes this call is
// supposed to publish.
type swapTempFileOnFirstRead struct {
	dir           string
	attackerBytes []byte
	inner         *bytes.Reader
	swapped       bool
}

func (s *swapTempFileOnFirstRead) Read(p []byte) (int, error) {
	if !s.swapped {
		s.swapped = true
		entries, err := os.ReadDir(s.dir)
		if err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), tempNamePrefix) {
					tmpPath := filepath.Join(s.dir, e.Name())
					_ = os.Remove(tmpPath)
					_ = os.WriteFile(tmpPath, s.attackerBytes, 0o644)
				}
			}
		}
	}
	return s.inner.Read(p)
}

// The temp file's random name stops being a secret the moment createTemp
// succeeds: it is a plain directory entry, visible to a listing from anything
// else with access to the same directory. Pinning the destination directory
// once (proven by TestRoot_WriteStreamAtomicPinsDestinationDirectoryOnce)
// says nothing about that — it protects which directory the name is resolved
// in, not what currently holds the name inside it. If the name is removed and
// recreated with different content while this call's own handle is still
// being written to, a rename addressed by name alone would publish whatever
// currently holds that name, not the bytes this call actually wrote.
func TestRoot_WriteStreamAtomicRefusesATempNameSwappedDuringTheCopy(t *testing.T) {
	dir := t.TempDir()
	const genuine = "the genuine bytes this call is publishing"
	const attacker = "ATTACKER-SUBSTITUTED CONTENT"
	src := &swapTempFileOnFirstRead{
		dir:           dir,
		attackerBytes: []byte(attacker),
		inner:         bytes.NewReader([]byte(genuine)),
	}

	r := openRoot(t, dir)
	err := r.WriteStreamAtomic("file.txt", src, 0o644)
	if !src.swapped {
		t.Fatal("the swap never ran; the window was not exercised")
	}
	if err == nil {
		t.Fatal("expected a refusal: the temp name was replaced with different content during the copy")
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "file.txt")); rerr == nil {
		if string(got) == attacker {
			t.Fatalf("attacker-substituted content was published under the requested name: %q", got)
		}
		t.Fatalf("file.txt was published despite the refusal: %q", got)
	}

	// The refusal's own cleanup must not delete the replacement either: by
	// the time cleanup runs, the name no longer refers to this call's file,
	// and removing it would delete whatever the replacement actually is —
	// the same "cannot unlink the exact object a handle refers to" hazard
	// documented on CreateExclusive, here applying to a name this call no
	// longer owns rather than one it never claimed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), tempNamePrefix) {
			continue
		}
		found = true
		got, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), rerr)
		}
		if string(got) != attacker {
			t.Fatalf("the surviving temp entry does not hold the replacement's content: %q", got)
		}
	}
	if !found {
		t.Fatal("cleanup removed the replacement file that took over the temp name")
	}
}

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
