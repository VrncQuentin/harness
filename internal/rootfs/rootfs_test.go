package rootfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. Junctions need no privilege
// and are traversed exactly like symlinks, so they exercise the same escape on
// machines where symlink creation is denied.
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

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestSetOpenReadsInRootFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", "a.txt"), "hello")

	target, err := Set{root}.Open(filepath.Join(root, "sub", "a.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	data, err := target.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("Read = %q, want %q", data, "hello")
	}
	if want := filepath.Join(root, "sub", "a.txt"); target.Display() != want {
		t.Errorf("Display = %q, want %q", target.Display(), want)
	}
}

func TestSetOpenSelectsTheOwningRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeFile(t, filepath.Join(second, "b.txt"), "second")

	target, err := Set{first, second}.Open(filepath.Join(second, "b.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	data, err := target.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("Read = %q, want %q", data, "second")
	}
}

func TestSetOpenAcceptsRelativeInput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "rel.txt"), "relative")
	t.Chdir(root)

	target, err := Set{root}.Open("rel.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	data, err := target.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "relative" {
		t.Errorf("Read = %q, want %q", data, "relative")
	}
	if !filepath.IsAbs(target.Display()) {
		t.Errorf("Display = %q, want an absolute path", target.Display())
	}
}

func TestSetOpenListsTheRootItself(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "one.txt"), "1")
	writeFile(t, filepath.Join(root, "two.txt"), "2")

	target, err := Set{root}.Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	entries, err := target.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("ReadDir returned %d entries, want 2", len(entries))
	}
}

// A directory listing goes straight into tool output, so two identical calls
// have to produce identical text. File.ReadDir hands back filesystem order; the
// os.ReadDir this replaced sorted.
func TestTargetReadDirIsSortedByName(t *testing.T) {
	root := t.TempDir()
	// Created in an order that is neither sorted nor reverse-sorted, so a
	// filesystem that happens to return creation order still fails an
	// unsorted implementation.
	for _, name := range []string{"middle.txt", "zeta.txt", "alpha.txt", "beta.txt"} {
		writeFile(t, filepath.Join(root, name), name)
	}
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	target, err := Set{root}.Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	entries, err := target.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"adir", "alpha.txt", "beta.txt", "middle.txt", "zeta.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("ReadDir = %v, want %v", got, want)
	}
}

func TestSetOpenRejectsPathsOutsideEveryRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "secret")
	sibling := root + "-sibling"
	writeFile(t, filepath.Join(sibling, "secret.txt"), "secret")

	tests := []struct {
		name string
		path string
	}{
		{name: "unrelated directory", path: filepath.Join(outside, "secret.txt")},
		{name: "sibling sharing a prefix", path: filepath.Join(sibling, "secret.txt")},
		{name: "the parent of the root", path: filepath.Dir(root)},
		{name: "traversal out of the root", path: filepath.Join(root, "..", "elsewhere.txt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := Set{root}.Open(tt.path)
			if err == nil {
				_ = target.Close()
				t.Fatalf("Open(%s) succeeded, want ErrOutsideRoots", tt.path)
			}
			if !errors.Is(err, ErrOutsideRoots) {
				t.Errorf("err = %v, want ErrOutsideRoots", err)
			}
		})
	}
}

func TestSetOpenRejectsEmptyRootList(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "a")

	for _, roots := range []Set{nil, {}, {"", "   "}} {
		target, err := roots.Open(filepath.Join(root, "a.txt"))
		if err == nil {
			_ = target.Close()
			t.Fatalf("roots %v accepted a path", roots)
		}
		if !errors.Is(err, ErrOutsideRoots) {
			t.Errorf("roots %v: err = %v, want ErrOutsideRoots", roots, err)
		}
	}
}

// An unresolvable path is not a path known to be outside — it is a path whose
// location is unknown, and the answer must be a refusal that says so rather
// than a containment verdict.
func TestSetOpenFailsClosedOnUnresolvablePaths(t *testing.T) {
	root := t.TempDir()

	target, err := Set{root}.Open(filepath.Join(root, "bad\x00name"))
	if err == nil {
		_ = target.Close()
		t.Fatal("Open accepted a path it could not resolve")
	}
	if errors.Is(err, ErrOutsideRoots) {
		t.Errorf("err = %v, want a resolution failure rather than a containment verdict", err)
	}

	target, err = Set{filepath.Join(root, "bad\x00root")}.Open(filepath.Join(root, "a.txt"))
	if err == nil {
		_ = target.Close()
		t.Fatal("Open accepted a call whose root could not be resolved")
	}
	if errors.Is(err, ErrOutsideRoots) {
		t.Errorf("err = %v, want a resolution failure rather than a containment verdict", err)
	}
}

// A link inside the root pointing out of it is the escape the sandbox exists to
// stop, whether or not the file it names has been created yet.
func TestSetOpenRejectsPathBelowLinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "SECRET")
	mustLinkDir(t, outside, filepath.Join(root, "leakdir"))

	tests := []string{
		filepath.Join(root, "leakdir", "secret.txt"),
		filepath.Join(root, "leakdir", "not-created-yet.txt"),
		filepath.Join(root, "leakdir"),
	}
	for _, path := range tests {
		target, err := Set{root}.Open(path)
		if err == nil {
			data, readErr := target.Read()
			_ = target.Close()
			t.Errorf("Open(%s) succeeded (read %q, err %v), want a refusal", path, data, readErr)
			continue
		}
		if !errors.Is(err, ErrOutsideRoots) {
			t.Errorf("Open(%s) err = %v, want ErrOutsideRoots", path, err)
		}
	}
}

// A root may itself be a link — Windows redirects several well-known
// directories that way — and resolving it must not lock the user out of it.
func TestSetOpenAllowsLinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeFile(t, filepath.Join(real, "a.txt"), "content")
	linked := filepath.Join(base, "linked")
	mustLinkDir(t, real, linked)

	target, err := Set{linked}.Open(filepath.Join(linked, "a.txt"))
	if err != nil {
		t.Fatalf("Open through a linked root: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	data, err := target.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("Read = %q, want %q", data, "content")
	}

	// The same file reached through the root's other spelling is the same file,
	// and must be accepted just the same.
	viaReal, err := Set{linked}.Open(filepath.Join(real, "a.txt"))
	if err != nil {
		t.Fatalf("Open the root's target spelling: %v", err)
	}
	_ = viaReal.Close()
}

// A link inside the root pointing back inside it must not lock the caller out
// of the file it names. os.Root refuses an absolute link target — which is what
// a Windows junction always stores — so Set addresses the target by the physical
// path pathid resolved rather than by the spelling that went through the link.
func TestSetOpenResolvesAnInRootLinkAway(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	writeFile(t, filepath.Join(real, "a.txt"), "in root")
	mustLinkDir(t, real, filepath.Join(root, "alias"))

	target, err := Set{root}.Open(filepath.Join(root, "alias", "a.txt"))
	if err != nil {
		t.Fatalf("Open through an in-root link: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	data, err := target.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "in root" {
		t.Errorf("Read = %q, want %q", data, "in root")
	}
}

func TestTargetReadReportsMissingFileAgainstTheDisplayPath(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.txt")

	target, err := Set{root}.Open(missing)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	_, err = target.Read()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read err = %v, want fs.ErrNotExist", err)
	}
	// os.Root names the root-relative path, which reads like a different file
	// to a caller that asked about an absolute one.
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name %s", err, missing)
	}
}

// Once the root is pinned, replacing the leaf with a link out of the root does
// not open the link's target: the resolution happens against the handle.
func TestTargetRefusesALeafSwappedForAnEscapingLink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "SECRET")
	sub := filepath.Join(root, "sub")
	writeFile(t, filepath.Join(sub, "ordinary.txt"), "ordinary")

	target, err := Set{root}.Open(sub)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	// Between the containment decision and the access, put a link out of the
	// root under the name that was checked.
	if err := os.RemoveAll(sub); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	mustLinkDir(t, outside, sub)

	entries, err := target.ReadDir()
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("ReadDir through a swapped-in escaping link succeeded: %v", names)
	}
}

// The window the review found: the root used to be resolved and authorized
// first and pinned afterwards, so a replacement staged in between was pinned
// instead, and the relative path authorized against the original was applied
// inside it. The pin now comes first and the identity is checked against the
// directory actually held open.
//
// The hook fires in exactly that window. Two ways to stage a replacement, one
// of which works on each platform.
func TestSetOpenRefusesARootReplacedWhileItIsAuthorized(t *testing.T) {
	const secret = "SECRET-REACHED-BY-REPLACING-THE-ROOT"

	// A link as the configured root, re-pointed after the pin. The pin follows
	// the link to the real directory, so re-pointing the link itself is not
	// blocked by the open handle and this runs everywhere.
	t.Run("configured root re-pointed", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		writeFile(t, filepath.Join(real, "f.txt"), "genuine")
		evil := filepath.Join(base, "evil")
		writeFile(t, filepath.Join(evil, "f.txt"), secret)
		root := filepath.Join(base, "root")
		mustLinkDir(t, real, root)

		swapped := false
		target, err := Set{root}.open(filepath.Join(root, "f.txt"), func() {
			if swapped {
				return
			}
			swapped = true
			if rmErr := os.Remove(root); rmErr != nil {
				t.Fatalf("Remove root link: %v", rmErr)
			}
			mustLinkDir(t, evil, root)
		})
		if !swapped {
			t.Fatal("the hook never ran; the window was not exercised")
		}
		if err == nil {
			data, readErr := target.Read()
			_ = target.Close()
			if string(data) == secret {
				t.Fatalf("a root replaced during authorization disclosed the attacker's file (read err %v)", readErr)
			}
			t.Fatalf("Open accepted a root that changed under it; it read %q", data)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("secret leaked through the error path: %v", err)
		}
		// Specifically the identity check, not an incidental failure.
		if !strings.Contains(err.Error(), "changed while it was being opened") {
			t.Errorf("err = %v, want the pinned-root identity refusal", err)
		}
	})

	// The same-name replacement: move the real directory aside and put the
	// attacker's under the name that was pinned. Renaming a directory with an
	// open handle is refused on Windows — where that refusal is itself the
	// defense — and permitted on Linux.
	t.Run("same-name replacement", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "root")
		writeFile(t, filepath.Join(root, "f.txt"), "genuine")
		evil := filepath.Join(base, "evil")
		writeFile(t, filepath.Join(evil, "f.txt"), secret)

		staged := false
		swapFailed := ""
		target, err := Set{root}.open(filepath.Join(root, "f.txt"), func() {
			if staged || swapFailed != "" {
				return
			}
			if rnErr := os.Rename(root, filepath.Join(base, "moved-aside")); rnErr != nil {
				swapFailed = rnErr.Error()
				return
			}
			if rnErr := os.Rename(evil, root); rnErr != nil {
				t.Fatalf("Rename evil into place: %v", rnErr)
			}
			staged = true
		})
		if !staged {
			if err == nil {
				_ = target.Close()
			}
			// Only Windows may decline to stage this, and there the refusal to
			// rename a pinned directory is the defense. Anywhere else a skip
			// would be an untested assertion that looks identical to a passing
			// one, so it fails instead.
			if runtime.GOOS != "windows" {
				t.Fatalf("could not stage the replacement this test exists to survive: %s", swapFailed)
			}
			t.Skipf("cannot rename a pinned directory here, which is itself the defense: %s", swapFailed)
		}
		if err == nil {
			data, _ := target.Read()
			_ = target.Close()
			if string(data) == secret {
				t.Fatal("a same-name replacement during authorization redirected the read")
			}
		}
	})
}

// The pathname is not part of the decision after the root is pinned: moving the
// real directory aside and putting an attacker's directory under the same name
// must not redirect anything.
//
// Renaming a directory with an open handle is refused on Windows and permitted
// on Linux, so this runs where it can be staged and skips elsewhere.
func TestTargetIgnoresSameNameRootReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	writeFile(t, filepath.Join(root, "f.txt"), "genuine")
	evil := filepath.Join(base, "evil")
	writeFile(t, filepath.Join(evil, "f.txt"), "SECRET")

	target, err := Set{root}.Open(filepath.Join(root, "f.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	if err := os.Rename(root, filepath.Join(base, "moved-aside")); err != nil {
		// See TestSetOpenRefusesARootReplacedWhileItIsAuthorized: a skip
		// anywhere but Windows would hide an assertion that never ran.
		if runtime.GOOS != "windows" {
			t.Fatalf("could not rename the pinned directory aside: %v", err)
		}
		t.Skipf("cannot rename a directory with an open handle here: %v", err)
	}
	if err := os.Rename(evil, root); err != nil {
		t.Fatalf("Rename evil into place: %v", err)
	}

	data, err := target.Read()
	if err != nil {
		return // refusing is also correct; disclosing is not
	}
	if string(data) == "SECRET" {
		t.Error("a same-name replacement of the root redirected the read")
	}
}

func TestTargetWriteAtomic(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	writeFile(t, existing, "before")

	target, err := Set{root}.Open(existing)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	if err := target.WriteAtomic([]byte("after"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "after" {
		t.Errorf("file = %q, want %q", got, "after")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempNamePrefix) {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

func TestTargetWriteAtomicCreatesThroughMissingParents(t *testing.T) {
	root := t.TempDir()
	target, err := Set{root}.Open(filepath.Join(root, "a", "b", "new.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	if err := target.MkdirAllParent(0o755); err != nil {
		t.Fatalf("MkdirAllParent: %v", err)
	}
	if err := target.WriteAtomic([]byte("created"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "a", "b", "new.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "created" {
		t.Errorf("file = %q, want %q", got, "created")
	}
}

// WriteAtomic publishes by rename and replaces whatever holds the name.
// CreateExclusive must not — it is the operation a create-only mode needs, and
// the difference is somebody else's file.
func TestTargetCreateExclusiveDoesNotReplace(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	writeFile(t, existing, "theirs")

	target, err := Set{root}.Open(existing)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	err = target.CreateExclusive([]byte("mine"), 0o644)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CreateExclusive over an existing file = %v, want fs.ErrExist", err)
	}
	got, readErr := os.ReadFile(existing)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if string(got) != "theirs" {
		t.Errorf("file = %q, want the existing content %q", got, "theirs")
	}

	// The same call on a free name succeeds.
	fresh, err := Set{root}.Open(filepath.Join(root, "fresh.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fresh.Close() //nolint:errcheck // test cleanup
	if err := fresh.CreateExclusive([]byte("mine"), 0o644); err != nil {
		t.Fatalf("CreateExclusive on a free name: %v", err)
	}
}

// Exactly one of a set of racing creates may win, and the file must hold that
// winner's bytes. A check-then-rename lets every caller believe it created the
// file while only the last one's content survives.
func TestTargetCreateExclusiveIsExclusiveUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "contended.txt")

	const writers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf("writer-%d", i)
			target, err := Set{root}.Open(path)
			if err != nil {
				return
			}
			defer target.Close() //nolint:errcheck // test cleanup
			if err := target.CreateExclusive([]byte(body), 0o644); err != nil {
				return
			}
			mu.Lock()
			winners = append(winners, body)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d writers reported creating the file, want exactly 1: %v", len(winners), winners)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != winners[0] {
		t.Errorf("file = %q but %q was told it created it", got, winners[0])
	}
}

// A directory swapped for a link out of the root between Open and the write
// must not carry the write with it.
func TestTargetWriteAtomicRefusesAnEscapingParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	target, err := Set{root}.Open(filepath.Join(sub, "written.txt"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer target.Close() //nolint:errcheck // test cleanup

	if err := os.RemoveAll(sub); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	mustLinkDir(t, outside, sub)

	if err := target.WriteAtomic([]byte("escaped"), 0o644); err == nil {
		t.Error("WriteAtomic followed a link out of the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "written.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was written outside the root: %v", err)
	}
}

func TestRootReadsAndClassifiesEntries(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "sub", "a.txt"), "body")

	root, err := Open(base)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer root.Close() //nolint:errcheck // test cleanup

	data, err := root.ReadFile(filepath.Join("sub", "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "body" {
		t.Errorf("ReadFile = %q, want %q", data, "body")
	}
	info, err := root.Lstat(filepath.Join("sub", "a.txt"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("Lstat mode = %v, want a regular file", info.Mode())
	}
	if _, err := root.ReadFile(filepath.Join("..", "escape.txt")); err == nil {
		t.Error("ReadFile followed a traversal out of the root")
	}
}

// ---------- WriteStreamAtomic regression tests ----------

func TestWriteStreamAtomic_PinSurvivesIntermediateSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory locking prevents mid-write swaps")
	}
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	// Set up: root/sub/orig/ (pinned parent) and root/sub/evil/
	orig := filepath.Join(dir, "sub", "orig")
	evil := filepath.Join(dir, "sub", "evil")
	if err := os.MkdirAll(orig, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write to sub/orig/file.txt through the root — this pins sub/orig.
	err = root.writeStreamAtomic("sub/orig/file.txt", bytes.NewReader([]byte("real")), 0o644,
		func(f *os.File, tmpRel string) {
			sub := filepath.Join(dir, "sub")
			if err := os.Rename(filepath.Join(sub, "orig"), filepath.Join(sub, "swapped")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(sub, "evil"), filepath.Join(sub, "orig")); err != nil {
				t.Fatal(err)
			}
		}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The file should land in the original (now swapped-aside) pinned
	// directory, because the child was pinned before the swap.
	swapped := filepath.Join(dir, "sub", "swapped")
	if _, err := os.Stat(filepath.Join(swapped, "file.txt")); err != nil {
		t.Error("file should be in the pinned (now swapped) directory:", err)
	}
	_ = os.RemoveAll(filepath.Join(dir, "sub"))
}

func TestWriteStreamAtomic_ReplacesHardLinkedLeaf(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(real, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}

	if err := root.WriteStreamAtomic("link.txt", bytes.NewReader([]byte("replaced")), 0o644); err != nil {
		t.Fatal(err)
	}

	linked, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(linked) != "original" {
		t.Error("hard-linked source should not be modified by rename publication")
	}
}

func TestWriteStreamAtomic_DetectsSubstitutedTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file locking prevents name-based substitution of open temp")
	}
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	var tmpPath string
	err = root.writeStreamAtomic("file.txt", bytes.NewReader([]byte("hello")), 0o644,
		func(f *os.File, tmpRel string) {
			tmpPath = filepath.Join(dir, tmpRel)
			if err := os.WriteFile(tmpPath+".new", []byte("impostor"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmpPath+".new", tmpPath); err != nil {
				t.Fatal(err)
			}
		}, nil)
	if err == nil {
		t.Fatal("expected error for substituted temp entry")
	}
	if !strings.Contains(err.Error(), "substituted") {
		t.Errorf("expected substitution error, got: %v", err)
	}
	// file.txt must not be published.
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("destination should not exist after substitution")
	}
	// The impostor should survive at the temp path.
	content, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "impostor" {
		t.Errorf("impostor should survive, got %s", string(content))
	}
}

func TestWriteStreamAtomic_DoesNotCleanUpTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	err = root.writeStreamAtomic("file.txt", &failingReader{data: "hello", failAfter: 3, err: errors.New("injected")}, 0o644, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	// The temp file should still exist with partial content.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".harness-write-") {
			content, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "hel" {
				t.Errorf("expected partial content 'hel', got '%s'", string(content))
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("partial temp file should survive error")
	}
}

type failingReader struct {
	data      string
	pos       int
	failAfter int
	err       error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.pos >= r.failAfter {
		return 0, r.err
	}
	if len(p) > 1 {
		p = p[:1]
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestWriteStreamAtomic_SyncBeforeRename(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	syncSentinel := errors.New("sync failed")
	err = root.writeStreamAtomic("file.txt", bytes.NewReader([]byte("hello")), 0o644, nil,
		func(f *os.File) error { return syncSentinel })
	if err == nil || !errors.Is(err, syncSentinel) {
		t.Errorf("sync hook error should propagate, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "file.txt")); !os.IsNotExist(err) {
		t.Error("destination should not exist after sync failure")
	}
}

func TestOpenChildNoFollow_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink requires Developer Mode on Windows")
		}
		t.Fatal(err)
	}

	_, _, err = root.OpenChildNoFollow("link")
	if err == nil {
		t.Error("OpenChildNoFollow should refuse symlink")
	}
}

func TestOpenChildNoFollow_AcceptsRealDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	child, fi, err := root.OpenChildNoFollow("sub")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Close() }()
	if !fi.IsDir() {
		t.Error("returned metadata should be a directory")
	}
	_, err = child.ReadDir(".")
	if err != nil {
		t.Fatal("child should be a valid directory:", err)
	}
}

func TestOpenChildNoFollow_RejectsMultiComponent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	_, _, err = root.OpenChildNoFollow(filepath.Join("a", "b"))
	if err == nil {
		t.Error("OpenChildNoFollow should reject multi-component path")
	}
}

func TestOpenChildNoFollow_DetectsSubstitution(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	real := filepath.Join(dir, "real")
	evil := filepath.Join(dir, "evil")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	_, _, err = root.openChildNoFollow("sub", func() {
		if err := os.Rename(evil, filepath.Join(dir, "sub")); err != nil {
			t.Skip("live handle blocked rename substitution")
		}
	})
	if err == nil {
		t.Error("OpenChildNoFollow should detect substitution")
	}
}
