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
				if got != "" {
					t.Errorf("got %q alongside an error, want empty", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantPath {
				t.Errorf("got %q, want %q", got, tt.wantPath)
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
	if !strings.HasSuffix(Key(got), Key(filepath.Join("not", "created", "yet.txt"))) {
		t.Errorf("Resolve = %q, want it to keep the missing tail", got)
	}
}

func TestWithinRoot(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"srv", "project")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "the root itself", path: root, want: true},
		{name: "a child", path: filepath.Join(root, "a", "b"), want: true},
		{name: "a sibling sharing a prefix", path: root + "-other"},
		{name: "an unrelated path", path: filepath.Join(string(filepath.Separator)+"srv", "other")},
		{name: "the parent", path: string(filepath.Separator) + "srv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WithinRoot(tt.path, root); got != tt.want {
				t.Errorf("WithinRoot(%q, %q) = %v, want %v", tt.path, root, got, tt.want)
			}
		})
	}

	if runtime.GOOS == "windows" && !WithinRoot(strings.ToUpper(root), root) {
		t.Error("WithinRoot is case-sensitive on Windows, where paths are not")
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
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if jerr != nil {
			t.Skipf("cannot create directory link: %v: %s", jerr, out)
		}
	}

	viaTarget, err := Resolve(target)
	if err != nil {
		t.Fatalf("Resolve target: %v", err)
	}
	viaLink, err := Resolve(link)
	if err != nil {
		t.Fatalf("Resolve link: %v", err)
	}
	if Key(viaTarget) != Key(viaLink) {
		t.Errorf("link and target resolved to different identities:\n  target: %s\n  link:   %s", viaTarget, viaLink)
	}
}

// CanonicalFile answers for the open description rather than a path, so a
// caller can validate the exact handle it reads from instead of canonicalizing
// a name and reopening it afterwards.
func TestCanonicalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	got, err := CanonicalFile(f)
	if err != nil {
		t.Fatalf("CanonicalFile: %v", err)
	}
	want, err := Canonical(path)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if Key(got) != Key(want) {
		t.Errorf("CanonicalFile = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, `\?\`) {
		t.Errorf("CanonicalFile = %q, still carries the extended-length prefix", got)
	}

	if _, err := CanonicalFile(nil); err == nil {
		t.Error("CanonicalFile(nil) returned no error")
	}
}

// A handle opened through a link must report where the file physically is, not
// the name it was reached by — that is the whole point of validating the
// handle.
func TestCanonicalFileSeesThroughLinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", alias, real).CombinedOutput()
		if jerr != nil {
			t.Skipf("cannot create directory link: %v: %s", jerr, out)
		}
	}

	f, err := os.Open(alias)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only handle

	got, err := CanonicalFile(f)
	if err != nil {
		t.Fatalf("CanonicalFile: %v", err)
	}
	want, err := Canonical(real)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if Key(got) != Key(want) {
		t.Errorf("handle opened via a link reported %q, want the physical %q", got, want)
	}
}
