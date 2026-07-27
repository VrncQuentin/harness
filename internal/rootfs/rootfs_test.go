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
		target, err := Set(roots).Open(filepath.Join(root, "a.txt"))
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

	if root.Name() != base {
		t.Errorf("Name = %q, want %q", root.Name(), base)
	}
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
