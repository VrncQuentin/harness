package pathid

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
// symlink and falling back to a Windows junction. Junctions need no special
// privilege and are traversed exactly like symlinks, so they exercise the same
// aliasing on developer machines where symlink creation is denied.
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

func TestCanonicalHasNoExtendedPrefix(t *testing.T) {
	dir := t.TempDir()
	got, err := Canonical(dir)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	// A \\?\-prefixed result would never compare equal to a user-configured
	// root, silently rejecting every path.
	if strings.HasPrefix(got, `\\?\`) {
		t.Errorf("Canonical = %q, still carries the extended-length prefix", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Canonical = %q, want an absolute path", got)
	}
}

func TestCanonicalMissingPathIsNotExist(t *testing.T) {
	// Resolve's upward walk keys on this classification. If a missing path
	// stopped reporting fs.ErrNotExist, every not-yet-created target would be
	// rejected instead of resolved through its parent.
	_, err := Canonical(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Canonical of a missing path = %v, want an fs.ErrNotExist", err)
	}
}

// Resolve walks upward only for a missing component. Any other failure has to
// propagate: treating it as absence would evaluate an existing but unreadable
// reparse point as though it were not there and return a lexical path that was
// never canonicalized.
func TestResolveErrorPolicy(t *testing.T) {
	root := t.TempDir()
	denied := errors.New("permission denied by test")

	tests := []struct {
		name      string
		canonical func(string) (string, error)
		path      string
		wantErr   error
		wantPath  string
	}{
		{
			name:      "existing path resolves",
			canonical: func(p string) (string, error) { return p, nil },
			path:      root,
			wantPath:  root,
		},
		{
			name: "missing leaf resolves through its parent",
			canonical: func(p string) (string, error) {
				if strings.HasSuffix(p, "missing") {
					return "", &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
				}
				return p, nil
			},
			path:     filepath.Join(root, "missing"),
			wantPath: filepath.Join(root, "missing"),
		},
		{
			name: "non-missing error propagates",
			canonical: func(p string) (string, error) {
				if strings.HasSuffix(p, "locked") {
					return "", &fs.PathError{Op: "open", Path: p, Err: denied}
				}
				return p, nil
			},
			path:    filepath.Join(root, "locked"),
			wantErr: denied,
		},
		{
			name: "non-missing error deep in the chain propagates",
			canonical: func(p string) (string, error) {
				if strings.Contains(p, "locked") {
					return "", &fs.PathError{Op: "open", Path: p, Err: denied}
				}
				return "", &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
			},
			path:    filepath.Join(root, "locked", "a", "b"),
			wantErr: denied,
		},
		{
			name: "everything missing up to the volume fails",
			canonical: func(p string) (string, error) {
				return "", &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
			},
			path:    filepath.Join(root, "a", "b"),
			wantErr: fs.ErrNotExist,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWith(tt.canonical, tt.path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if !got.IsZero() {
					t.Errorf("got %q alongside an error, want the zero ID", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Path() != tt.wantPath {
				t.Errorf("got %q, want %q", got.Path(), tt.wantPath)
			}
		})
	}
}

func TestResolveRejectsUnresolvableName(t *testing.T) {
	// A NUL byte is rejected by the OS with something other than "not found"
	// on both platforms, so it reaches the propagation path through the real
	// canonicalizer rather than a stub.
	if _, err := Resolve(filepath.Join(t.TempDir(), "bad\x00name")); err == nil {
		t.Error("Resolve accepted a name the OS cannot evaluate")
	}
}

func TestResolveReappendsMissingTail(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "not", "created", "yet.txt")

	got, err := Resolve(target)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Returning only the existing ancestor would compare a shorter path than
	// the caller asked about.
	tail := newID(filepath.Join(string(filepath.Separator), "not", "created", "yet.txt"))
	if !strings.HasSuffix(got.Key(), tail.Key()) {
		t.Errorf("Resolve = %q, want it to keep the missing tail", got)
	}
}

// A relative spelling and an absolute one name one place. Resolve returning a
// relative path — which filepath.EvalSymlinks does for a relative input —
// would produce an identity that compares equal to nothing.
func TestResolveIsAbsoluteForRelativeInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(dir)

	cases := []struct{ name, rel, abs string }{
		{name: "existing file", rel: "a.txt", abs: filepath.Join(dir, "a.txt")},
		{name: "missing file", rel: "missing.txt", abs: filepath.Join(dir, "missing.txt")},
		{name: "dot", rel: ".", abs: dir},
		{name: "dot-prefixed", rel: filepath.Join(".", "a.txt"), abs: filepath.Join(dir, "a.txt")},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			relID, err := Resolve(tt.rel)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.rel, err)
			}
			if !filepath.IsAbs(relID.Path()) {
				t.Fatalf("Resolve(%q) = %q, want an absolute path", tt.rel, relID.Path())
			}
			absID, err := Resolve(tt.abs)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.abs, err)
			}
			if !relID.Equal(absID) {
				t.Errorf("relative %q and absolute %q resolved to different identities:\n  %s\n  %s",
					tt.rel, tt.abs, relID, absID)
			}
		})
	}
}

func TestIDContains(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Join(sep+"srv", "project")

	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "the root itself", root: root, path: root, want: true},
		{name: "a child", root: root, path: filepath.Join(root, "a", "b"), want: true},
		{name: "a sibling sharing a prefix", root: root, path: root + "-other"},
		{name: "an unrelated path", root: root, path: filepath.Join(sep+"srv", "other")},
		{name: "the parent", root: root, path: sep + "srv"},
		// A filesystem root already ends in a separator. The prefix test this
		// replaced compared against root+separator and so contained nothing.
		{name: "filesystem root holds a descendant", root: sep, path: root, want: true},
		{name: "filesystem root holds itself", root: sep, path: sep, want: true},
		{name: "a descendant does not hold its root", root: root, path: sep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newID(tt.root).Contains(newID(tt.path)); got != tt.want {
				t.Errorf("%q.Contains(%q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}

	if got := (ID{}).Contains(newID(root)); got {
		t.Error("the zero ID contains something")
	}
	if got := newID(root).Contains(ID{}); got {
		t.Error("something contains the zero ID")
	}
}

func TestIDContainsWindowsVolumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows volume semantics")
	}
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "drive root holds a child", root: `C:\`, path: `C:\srv`, want: true},
		{name: "drive root holds itself", root: `C:\`, path: `C:\`, want: true},
		{name: "different volumes", root: `C:\srv`, path: `D:\srv`},
		{name: "different volume roots", root: `C:\`, path: `D:\`},
		{name: "case-insensitive descendant", root: `C:\Srv\Project`, path: `c:\srv\project\a`, want: true},
		{name: "UNC share holds a child", root: `\\server\share`, path: `\\server\share\a`, want: true},
		{name: "different UNC shares", root: `\\server\share`, path: `\\server\second\a`},
		{name: "UNC does not hold a drive path", root: `\\server\share`, path: `C:\a`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newID(tt.root).Contains(newID(tt.path)); got != tt.want {
				t.Errorf("%q.Contains(%q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

func TestIDContainsIsCaseSensitiveOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix path casing")
	}
	if newID("/srv/Project").Contains(newID("/srv/project/a")) {
		t.Error("containment ignored case on a case-sensitive platform")
	}
}

func TestSame(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	for _, d := range []string{repo, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	if same, err := Same(repo, filepath.Join(base, ".", "repo")); err != nil || !same {
		t.Errorf("Same(repo, dotted repo) = (%v, %v), want (true, nil)", same, err)
	}
	if same, err := Same(repo, other); err != nil || same {
		t.Errorf("Same(repo, other) = (%v, %v), want (false, nil)", same, err)
	}
	// An unresolvable side is an error, never a quiet false: "I cannot locate
	// this" must not reach a caller as "these are different places".
	if _, err := Same(filepath.Join(base, "bad\x00name"), repo); err == nil {
		t.Error("Same returned no error for an unresolvable first path")
	}
	if _, err := Same(repo, filepath.Join(base, "bad\x00name")); err == nil {
		t.Error("Same returned no error for an unresolvable second path")
	}
}

func TestSameOrWithin(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	for _, d := range []string{repo, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	if in, err := SameOrWithin(filepath.Join(repo, "sub"), []string{repo}); err != nil || !in {
		t.Errorf("SameOrWithin(child) = (%v, %v), want (true, nil)", in, err)
	}
	if in, err := SameOrWithin(other, []string{repo}); err != nil || in {
		t.Errorf("SameOrWithin(unrelated) = (%v, %v), want (false, nil)", in, err)
	}
	if in, err := SameOrWithin(other, []string{"", "   "}); err != nil || in {
		t.Errorf("SameOrWithin(blank roots) = (%v, %v), want (false, nil)", in, err)
	}
	// A sibling sharing a textual prefix is outside, not inside.
	if in, err := SameOrWithin(repo+"-sibling", []string{repo}); err != nil || in {
		t.Errorf("SameOrWithin(prefix sibling) = (%v, %v), want (false, nil)", in, err)
	}
	// An unresolvable side is an error, never a quiet false.
	if _, err := SameOrWithin(filepath.Join(base, "bad\x00name"), []string{repo}); err == nil {
		t.Error("SameOrWithin returned no error for an unresolvable path")
	}
	if _, err := SameOrWithin(repo, []string{filepath.Join(base, "bad\x00root")}); err == nil {
		t.Error("SameOrWithin returned no error for an unresolvable root")
	}
}

// A link and its target are the same place and must produce the same identity,
// or a lock keyed on the path hands out two mutexes for one repository.
func TestResolveGivesLinkAndTargetOneIdentity(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(base, "alias")
	mustLinkDir(t, target, link)

	same, err := Same(target, link)
	if err != nil {
		t.Fatalf("Same: %v", err)
	}
	if !same {
		t.Error("a link and its target resolved to different identities")
	}

	// The same must hold for a path below the alias, including one that does
	// not exist yet.
	for _, name := range []string{"file.txt", "missing.txt"} {
		same, err := Same(filepath.Join(target, name), filepath.Join(link, name))
		if err != nil {
			t.Fatalf("Same(%s): %v", name, err)
		}
		if !same {
			t.Errorf("%s below the link and below the target resolved differently", name)
		}
	}
}

// Two spellings of one not-yet-created file are the same identity but not the
// same struct: Resolve re-appends the missing tail in the caller's case. Key is
// the map key; the ID is not.
func TestIDIsNotAMapKey(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("needs a case-insensitive filesystem")
	}
	dir := t.TempDir()
	lower, err := Resolve(filepath.Join(dir, "missing.txt"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	upper, err := Resolve(filepath.Join(dir, "MISSING.TXT"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !lower.Equal(upper) {
		t.Fatalf("%s and %s are the same file but compared unequal", lower, upper)
	}
	if lower.Key() != upper.Key() {
		t.Errorf("Key differs for one identity: %q vs %q", lower.Key(), upper.Key())
	}
	if lower == upper {
		t.Skip("this platform happened to produce identical structs; the Key contract still stands")
	}
	byKey := map[string]int{lower.Key(): 1}
	byKey[upper.Key()]++
	if len(byKey) != 1 {
		t.Errorf("keying by Key produced %d entries for one identity", len(byKey))
	}
}
