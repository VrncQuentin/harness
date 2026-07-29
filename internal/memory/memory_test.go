package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newTestRepo builds a memory repo under t.TempDir() from a map of
// relative paths to file contents. Parent directories are created
// automatically.
func newTestRepo(t *testing.T, files map[string]string) *DirReader {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", abs, err)
		}
	}
	r, err := NewDirReader(root)
	if err != nil {
		t.Fatalf("NewDirReader: %v", err)
	}
	return r
}

func TestDirReader_Read(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "hello rules",
	})

	got, err := r.Read("global/rules.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello rules" {
		t.Errorf("Read: got %q, want %q", string(got), "hello rules")
	}
}

func TestDirReader_ReadMissingReturnsFsErrNotExist(t *testing.T) {
	r := newTestRepo(t, nil)
	_, err := r.Read("global/rules.md")
	if err == nil {
		t.Fatal("Read: expected error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read missing file: errors.Is(err, fs.ErrNotExist) = false, err = %v", err)
	}
}

func TestDirReader_ReadRejectsTraversal(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "x",
	})

	tests := []struct {
		name string
		path string
	}{
		{"dotdot prefix", "../secret"},
		{"dotdot middle", "global/../../etc/passwd"},
		{"backslash dotdot", "global\\..\\..\\etc"},
		{"empty", ""},
		{"unix absolute", "/etc/passwd"},
		{"windows absolute", "C:/windows/system32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Read(tc.path)
			if err == nil {
				t.Fatalf("Read(%q): expected error, got nil", tc.path)
			}
		})
	}
}
func TestDirReader_Glob(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/episodes/2026-01-01.md": "one",
		"agents/coder/episodes/2026-02-01.md": "two",
		"agents/coder/episodes/2025-12-01.md": "zero",
		"agents/coder/episodes/notes.txt":     "ignore",
		"agents/coder/persona.md":             "persona",
	})

	got, err := r.Glob("agents/coder/episodes/*.md")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	want := []string{
		"agents/coder/episodes/2025-12-01.md",
		"agents/coder/episodes/2026-01-01.md",
		"agents/coder/episodes/2026-02-01.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Glob =\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestDirReader_GlobMissingParent(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "x",
	})
	got, err := r.Glob("agents/coder/episodes/*.md")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Glob on missing parent = %v, want []", got)
	}
}

func TestDirReader_GlobSkipsDirectories(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/episodes/real.md":       "x",
		"agents/coder/episodes/sub/nested.md": "y", // inside a subdir, not matched at this level
	})
	got, err := r.Glob("agents/coder/episodes/*")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	// Directories should not appear in the result list.
	for _, p := range got {
		if p == "agents/coder/episodes/sub" {
			t.Errorf("Glob returned directory: %q", p)
		}
	}
}

func TestDirReader_GlobRejectsTraversal(t *testing.T) {
	r := newTestRepo(t, nil)
	if _, err := r.Glob("../*.md"); err == nil {
		t.Error("Glob with traversal: expected error, got nil")
	}
}

func TestDirReader_ListDirs(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md":    "x",
		"agents/reviewer/persona.md": "y",
		"agents/README.md":           "z", // file at the enumerated level, skipped
		"agents/zeta/notes.md":       "n",
	})

	got, err := r.ListDirs("agents")
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	want := []string{"coder", "reviewer", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListDirs = %v, want %v", got, want)
	}
}

func TestDirReader_ListDirsMissing(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "x",
	})
	got, err := r.ListDirs("agents")
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListDirs on missing dir = %v, want []", got)
	}
}

func TestDirReader_ListDirsRejectsTraversal(t *testing.T) {
	r := newTestRepo(t, nil)
	if _, err := r.ListDirs("../outside"); err == nil {
		t.Error("ListDirs with traversal: expected error, got nil")
	}
}

func TestDirReader_MkdirAllCreatesNested(t *testing.T) {
	r := newTestRepo(t, nil)
	if err := r.MkdirAll("agents/coder"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	got, err := r.ListDirs("agents")
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"coder"}) {
		t.Errorf("ListDirs after MkdirAll = %v, want [coder]", got)
	}
}

func TestDirReader_MkdirAllIdempotent(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md": "x",
	})
	if err := r.MkdirAll("agents/coder"); err != nil {
		t.Fatalf("MkdirAll on existing dir: %v", err)
	}
	// File under it should be untouched.
	body, err := r.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(body) != "x" {
		t.Errorf("MkdirAll clobbered file: got %q, want %q", string(body), "x")
	}
}

func TestDirReader_MkdirAllRejectsTraversal(t *testing.T) {
	r := newTestRepo(t, nil)
	tests := []string{"", "../outside", "/etc/passwd", "agents/../..", "agents/..\\..\\etc"}
	for _, p := range tests {
		t.Run(p, func(t *testing.T) {
			if err := r.MkdirAll(p); err == nil {
				t.Errorf("MkdirAll(%q): expected error, got nil", p)
			}
		})
	}
}

func TestDirReader_MkdirAllOverFileFails(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder": "this is a file, not a dir",
	})
	if err := r.MkdirAll("agents/coder"); err == nil {
		t.Error("MkdirAll over a file: expected error, got nil")
	}
}

func TestDirReader_WriteFileCreatesNew(t *testing.T) {
	r := newTestRepo(t, nil)
	if err := r.WriteFile("agents/coder/persona.md", []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := r.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("Read after WriteFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("Read after WriteFile = %q, want %q", string(got), "hello")
	}
}

func TestDirReader_WriteFileOverwritesAtomic(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md": "old",
	})
	if err := r.WriteFile("agents/coder/persona.md", []byte("new")); err != nil {
		t.Fatalf("WriteFile overwrite: %v", err)
	}
	got, err := r.Read("agents/coder/persona.md")
	if err != nil {
		t.Fatalf("Read after WriteFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("Read after WriteFile = %q, want %q", string(got), "new")
	}

	// Confirm the atomic rename did not leave a .harness-* tempfile behind.
	parent := filepath.Join(r.root, "agents", "coder")
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("ReadDir parent: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".harness-") {
			t.Errorf("WriteFile left tempfile behind: %s", e.Name())
		}
	}
}

func TestDirReader_WriteFileCreatesParentDir(t *testing.T) {
	r := newTestRepo(t, nil)
	if err := r.WriteFile("agents/new/notes.md", []byte("body")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := r.Read("agents/new/notes.md")
	if err != nil {
		t.Fatalf("Read after WriteFile: %v", err)
	}
	if string(got) != "body" {
		t.Errorf("Read after WriteFile = %q, want %q", string(got), "body")
	}
}

func TestDirReader_WriteFileRejectsBadPaths(t *testing.T) {
	r := newTestRepo(t, nil)
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"dotdot prefix", "../secret"},
		{"dotdot middle", "agents/../../etc/passwd"},
		{"backslash dotdot", "agents\\..\\..\\etc"},
		{"unix absolute", "/etc/passwd"},
		{"windows absolute", "C:/windows/system32"},
		{"windows absolute backslash", "C:\\windows\\system32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.WriteFile(tc.path, []byte("x")); err == nil {
				t.Errorf("WriteFile(%q): expected error, got nil", tc.path)
			}
		})
	}
}

func TestDirReader_WriteFileOverDirectoryFails(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md": "x",
	})
	// Targeting an existing directory must fail; the rename cannot
	// replace a non-empty directory with a regular file on any OS.
	if err := r.WriteFile("agents/coder", []byte("body")); err == nil {
		t.Error("WriteFile over a directory: expected error, got nil")
	}
}

func TestDirReader_RemoveAllRemovesSubtree(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md":          "p",
		"agents/coder/notes.md":            "n",
		"agents/coder/episodes/2026-01.md": "e1",
		"agents/reviewer/persona.md":       "r",
	})
	if err := r.RemoveAll("agents/coder"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	got, err := r.ListDirs("agents")
	if err != nil {
		t.Fatalf("ListDirs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"reviewer"}) {
		t.Errorf("ListDirs after RemoveAll = %v, want [reviewer]", got)
	}
	if _, err := os.Stat(filepath.Join(r.root, "agents", "coder")); !os.IsNotExist(err) {
		t.Errorf("agents/coder still exists: stat err = %v", err)
	}
}

func TestDirReader_RemoveAllMissingPathIsNoError(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "x",
	})
	if err := r.RemoveAll("agents/coder"); err != nil {
		t.Errorf("RemoveAll on missing path: %v", err)
	}
}

func TestDirReader_RemoveAllRejectsBadPaths(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"agents/coder/persona.md": "x",
	})
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"dotdot prefix", "../outside"},
		{"dotdot middle", "agents/../../etc"},
		{"backslash dotdot", "agents\\..\\..\\etc"},
		{"unix absolute", "/etc/passwd"},
		{"windows absolute", "C:/windows/system32"},
		{"windows absolute backslash", "C:\\windows\\system32"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.RemoveAll(tc.path); err == nil {
				t.Errorf("RemoveAll(%q): expected error, got nil", tc.path)
			}
		})
	}
	// The well-formed file the bad inputs were trying to dodge into must
	// still be readable - i.e. nothing was deleted in the rejection path.
	if got, err := r.Read("agents/coder/persona.md"); err != nil || string(got) != "x" {
		t.Errorf("persona.md tampered: got %q err=%v, want %q", string(got), err, "x")
	}
}

func TestDirReader_WalkReturnsEverythingSorted(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md":         "rules",
		"global/user.md":          "user",
		"agents/coder/persona.md": "p",
		"agents/coder/notes.md":   "n",
	})

	got, err := r.Walk("")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	wantPaths := []string{
		"agents",
		"agents/coder",
		"agents/coder/notes.md",
		"agents/coder/persona.md",
		"global",
		"global/rules.md",
		"global/user.md",
	}
	gotPaths := make([]string, len(got))
	for i, e := range got {
		gotPaths[i] = e.Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("Walk paths =\n\t%v\nwant\n\t%v", gotPaths, wantPaths)
	}
	// Spot-check: rules.md must report a non-zero size and Dir=false.
	for _, e := range got {
		if e.Path == "global/rules.md" {
			if e.Dir {
				t.Error("Walk: global/rules.md reported Dir=true")
			}
			if e.Size != int64(len("rules")) {
				t.Errorf("Walk: global/rules.md size=%d, want %d", e.Size, len("rules"))
			}
		}
		if e.Path == "global" && !e.Dir {
			t.Error("Walk: global reported Dir=false")
		}
	}
}

func TestDirReader_WalkSkipsGitDir(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"global/rules.md": "x",
		".git/HEAD":       "ref: refs/heads/main",
		".git/config":     "[core]",
	})
	got, err := r.Walk("")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, e := range got {
		if strings.HasPrefix(e.Path, ".git") {
			t.Errorf("Walk leaked git plumbing: %s", e.Path)
		}
	}
}

func TestDirReader_WalkMissingRoot(t *testing.T) {
	_, err := NewDirReader(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Error("NewDirReader should fail when the root directory does not exist")
	}
}

func TestDirReader_WalkRejectsTraversal(t *testing.T) {
	r := newTestRepo(t, nil)
	if _, err := r.Walk("../outside"); err == nil {
		t.Error("Walk with traversal: expected error, got nil")
	}
}

func TestDirReader_ReadDoesNotFollowLink(t *testing.T) {
	dir := t.TempDir()
	// Create two directories: the repo root and an outside dir.
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.txt"), []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Place a symlink inside the repo pointing outside.
	if err := os.Symlink(outside, filepath.Join(repoRoot, "link")); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Read through the link should be refused because the anchor's
	// os.Root containment rejects absolute targets.
	_, err = r.Read("link/evil.txt")
	if err == nil {
		t.Error("Read through link should fail")
	}
}

func TestDirReader_WalkDoesNotEnterSymlink(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "link")); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Walk should not descend into the symlink (OpenChildNoFollow refuses).
	entries, err := r.Walk("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "link/") {
			t.Errorf("Walk descended into symlink: %s", e.Path)
		}
	}
}

func TestDirReader_GlobDoesNotFollowLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.txt"), []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "link")); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Glob("link/*.txt")
	if err == nil {
		t.Error("Glob through link should fail")
	}
}

func TestDirReader_ListDirsDoesNotFollowLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.txt"), []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoRoot, "link")); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// ListDirs("") lists the repo root; the symlink may appear as an
	// entry.  Listing the symlink itself should fail because its
	// target is outside the anchored root.
	_, err = r.ListDirs("link")
	if err == nil {
		t.Error("ListDirs through link should fail")
	}
}

func TestDirReader_WalkKeepsDescendingInsidePinnedTree(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repoRoot, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "sub", "deep", "file.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a junction/symlink alias, construct the reader through
	// it, and verify Walk descends correctly through the physical tree.
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(repoRoot, alias); err != nil {
		t.Skip("symlink unavailable: " + err.Error())
	}
	r, err := NewDirReader(alias)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := r.Walk("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Path == "sub/deep/file.txt" {
			found = true
		}
	}
	if !found {
		t.Error("Walk should descend into nested directories inside the pinned tree")
	}
}

func TestDirReader_WriteFileRejectsRootTarget(t *testing.T) {
	r, err := NewDirReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(".", []byte("x")); err == nil {
		t.Error("WriteFile('.') should be rejected")
	}
	if err := r.WriteFile("./", []byte("x")); err == nil {
		t.Error("WriteFile('./') should be rejected")
	}
}

func TestDirReader_WriteFileReplacesHardLinkedLeaf(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(real, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := NewDirReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile("link.txt", []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	// The hard-linked source must not be modified.
	linked, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(linked) != "original" {
		t.Error("hard-linked source should not be modified by rename publication")
	}
	// The link name should contain the new content.
	linkContent, err := os.ReadFile(filepath.Join(dir, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(linkContent) != "replaced" {
		t.Error("link name should contain the new content")
	}
}

func TestDirReader_WriteFileRefusesLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	mustLinkDir(t, outside, filepath.Join(repoRoot, "link"))
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = r.WriteFile("link/evil.txt", []byte("evil"))
	if err == nil {
		t.Error("WriteFile through link should fail")
	}
}

func TestDirReader_MkdirAllRefusesLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	mustLinkDir(t, outside, filepath.Join(repoRoot, "link"))
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = r.MkdirAll("link/subdir")
	if err == nil {
		t.Error("MkdirAll through link should fail")
	}
}

func TestDirReader_RemoveAllRefusesLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "victim", "f.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLinkDir(t, outside, filepath.Join(repoRoot, "link"))
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = r.RemoveAll("link/victim")
	if err == nil {
		t.Error("RemoveAll through link should fail")
	}
	// The outside victim must survive.
	content, err := os.ReadFile(filepath.Join(outside, "victim", "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Error("outside victim should survive failed RemoveAll")
	}
}

func TestDirReader_WriteFileCleansTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	r, err := NewDirReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	// WriteFile("a/b/") with a trailing slash should publish at "a/b"
	// not create "a/b/b".
	if err := r.WriteFile("a/b/", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Errorf("WriteFile('a/b/') should publish at a/b, got content=%q", string(got))
	}
}
