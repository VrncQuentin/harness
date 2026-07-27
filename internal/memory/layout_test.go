package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

func TestCreateMissing_All(t *testing.T) {
	root := t.TempDir()
	if err := CreateMissing(root, ExpectedProjectRepoLayout(false)); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}

	// Re-check: after scaffolding everything, MissingProjectRepoItems must be empty.
	missing, err := MissingProjectRepoItems(root, false)
	if err != nil {
		t.Fatalf("MissingProjectRepoItems after scaffold: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("MissingProjectRepoItems after scaffold: got %v, want []", missing)
	}

	// File items must be created as zero-byte files; directory items
	// must be directories.
	for _, item := range ExpectedProjectRepoLayout(false) {
		abs := filepath.Join(root, filepath.FromSlash(item.Path))
		st, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("Stat %s: %v", item.Path, err)
		}
		if st.IsDir() != item.Dir {
			t.Errorf("scaffold %s: IsDir=%v, want %v", item.Path, st.IsDir(), item.Dir)
		}
		if !item.Dir && st.Size() != 0 {
			t.Errorf("scaffold %s: size=%d, want 0 (empty file)", item.Path, st.Size())
		}
	}
}

func TestCreateMissing_PreservesExistingContent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rules := filepath.Join(root, "rules.md")
	if err := os.WriteFile(rules, []byte("user wrote this"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := CreateMissing(root, ExpectedProjectRepoLayout(false)); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}

	got, err := os.ReadFile(rules)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "user wrote this" {
		t.Errorf("CreateMissing clobbered existing file: got %q", string(got))
	}
}

func TestCreateMissing_LeavesWrongKindAlone(t *testing.T) {
	root := t.TempDir()
	// File where a directory is expected - must NOT be removed.
	bogus := filepath.Join(root, "agents")
	if err := os.WriteFile(bogus, []byte("user data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	missing, err := MissingProjectRepoItems(root, false)
	if err != nil {
		t.Fatalf("MissingProjectRepoItems: %v", err)
	}
	// CreateMissing may surface an error here because children of
	// agents/ cannot be created while agents is a file; the contract
	// is only that user data is never overwritten or removed.
	_ = CreateMissing(root, missing)

	got, err := os.ReadFile(bogus)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "user data" {
		t.Errorf("CreateMissing destroyed wrong-kind data: got %q", string(got))
	}
}

func TestCreateMissing_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	tests := []LayoutItem{
		{Path: "../escape", Dir: false},
		{Path: "global/../../etc", Dir: false},
		{Path: "/abs/path", Dir: true},
		{Path: "C:/windows", Dir: false},
		{Path: "C:\\windows", Dir: false},
		{Path: "", Dir: false},
	}
	for _, tc := range tests {
		t.Run(tc.Path, func(t *testing.T) {
			if err := CreateMissing(root, []LayoutItem{tc}); err == nil {
				t.Errorf("CreateMissing(%q): expected error, got nil", tc.Path)
			}
		})
	}
}

func TestCreateMissing_EmptyPath(t *testing.T) {
	if err := CreateMissing("", ExpectedProjectRepoLayout(false)); err == nil {
		t.Error("CreateMissing(\"\"): expected error, got nil")
	}
}

func TestCreateMissing_NonexistentRoot(t *testing.T) {
	if err := CreateMissing(filepath.Join(t.TempDir(), "does-not-exist"), ExpectedProjectRepoLayout(false)); err == nil {
		t.Error("CreateMissing on missing root: expected error, got nil")
	}
}

func TestProjectScaffoldServiceCreateMissing(t *testing.T) {
	root := t.TempDir()
	service := ProjectScaffoldService{}
	created, err := service.CreateMissing(root, false)
	if err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	if created == 0 {
		t.Fatal("CreateMissing created 0 entries, want scaffold files")
	}
	missing, err := service.Missing(root, false)
	if err != nil {
		t.Fatalf("Missing after CreateMissing: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing after scaffold = %v", missing)
	}
	created, err = service.CreateMissing(root, false)
	if err != nil {
		t.Fatalf("CreateMissing complete repo: %v", err)
	}
	if created != 0 {
		t.Fatalf("CreateMissing complete repo created %d entries, want 0", created)
	}
}
func TestCreateMissingProjectRepoWritesGitkeep(t *testing.T) {
	root := t.TempDir()
	if err := CreateMissingProjectRepo(root, true); err != nil {
		t.Fatalf("CreateMissingProjectRepo: %v", err)
	}
	for _, rel := range []string{"agents/.gitkeep", "episodes/.gitkeep", "index/.gitkeep", "index/_episodes/.gitkeep", "artifacts/.gitkeep"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
}

func TestEnsureProjectRepoInitializesAndScaffolds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project-repo")
	if err := EnsureProjectRepo(root, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	for _, rel := range []string{".git", "rules.md", "user.md", "facts.md", "agents/.gitkeep", "sessions.jsonl", "episodes/.gitkeep", "index/_episodes/.gitkeep"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
	if err := ValidateProjectRepo(root, false); err != nil {
		t.Fatalf("ValidateProjectRepo: %v", err)
	}
}

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction, which needs no privilege and
// is traversed the same way. The test is skipped when neither is available.
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

func TestSameProjectRepoPathUsesOSPathIdentity(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "Repo")
	b := filepath.Join(base, "repo")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	got, err := SameProjectRepoPath(a, b)
	if err != nil {
		t.Fatalf("SameProjectRepoPath: %v", err)
	}
	if runtime.GOOS == "windows" {
		if !got {
			t.Fatalf("SameProjectRepoPath(%q, %q) = false on Windows, want true", a, b)
		}
		return
	}
	if got {
		t.Fatalf("SameProjectRepoPath(%q, %q) = true on %s, want false", a, b, runtime.GOOS)
	}
}

// A link and the directory it points at are one repository. The lexical
// comparison this replaced called them different, and "different" is what makes
// MoveProjectRepo copy over a destination.
func TestSameProjectRepoPathRecognisesAlias(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, real, alias)

	same, err := SameProjectRepoPath(real, alias)
	if err != nil {
		t.Fatalf("SameProjectRepoPath: %v", err)
	}
	if !same {
		t.Error("a link and its target were not recognised as one repository")
	}

	unrelated := filepath.Join(base, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	same, err = SameProjectRepoPath(real, unrelated)
	if err != nil {
		t.Fatalf("SameProjectRepoPath: %v", err)
	}
	if same {
		t.Error("two distinct directories were recognised as one repository")
	}
}

func TestSameProjectRepoPathFailsClosedOnUnresolvablePath(t *testing.T) {
	base := t.TempDir()
	if _, err := SameProjectRepoPath(filepath.Join(base, "bad\x00name"), base); err == nil {
		t.Error("SameProjectRepoPath returned no error for a path it could not resolve")
	}
}

// A destination that names the source through another spelling is the source.
// Copying then walks the tree opening every destination file with O_TRUNC —
// the same file — so the repository is emptied one file at a time and each
// copy reads back from the file it has just truncated.
//
// Recognising the alias may either collapse the move into an ensure or refuse
// it outright: go-git declines to open a repository through a reparse point.
// Both outcomes are correct. What must never happen is the copy.
func TestMoveProjectRepoDoesNotCopyARepoOntoItself(t *testing.T) {
	const body = "keep me"

	newRepo := func(t *testing.T) (base, src, notes string) {
		t.Helper()
		base = t.TempDir()
		src = filepath.Join(base, "src")
		if err := EnsureProjectRepo(src, false); err != nil {
			t.Fatalf("EnsureProjectRepo: %v", err)
		}
		notes = filepath.Join(src, "notes.md")
		if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
			t.Fatalf("write notes: %v", err)
		}
		return base, src, notes
	}
	assertIntact := func(t *testing.T, notes string) {
		t.Helper()
		got, err := os.ReadFile(notes)
		if err != nil {
			t.Fatalf("read notes after the self-move: %v", err)
		}
		if string(got) != body {
			t.Fatalf("notes.md = %q after moving the repo onto an alias of itself, want %q", got, body)
		}
	}

	t.Run("directory link", func(t *testing.T) {
		base, src, notes := newRepo(t)
		alias := filepath.Join(base, "alias")
		mustLinkDir(t, src, alias)

		_ = MoveProjectRepo(src, alias, false)
		assertIntact(t, notes)
	})

	t.Run("windows case alias", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("windows path casing")
		}
		_, src, notes := newRepo(t)
		if err := MoveProjectRepo(src, strings.ToUpper(src), false); err != nil {
			t.Fatalf("MoveProjectRepo onto an upper-cased spelling of itself: %v", err)
		}
		assertIntact(t, notes)
	})
}

// MoveProjectRepo's name-based identity check runs once, before the copy. A
// destination re-pointed at the source after that check leaves exactly the
// state this test sets up: two names the check saw as different that now name
// one directory. Calling the copy directly reproduces it without needing to win
// a race.
//
// Without the handle comparison the copy opens every destination file with
// O_TRUNC — the same file it is about to read — so the repository is emptied
// one file at a time.
func TestCopyTreeWithoutGitRefusesAnAliasedDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	const body = "keep me"
	notes := filepath.Join(src, "notes.md")
	if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, src, alias)

	err := copyTreeWithoutGit(src, alias)
	if err == nil {
		t.Error("copied a project memory repo onto an alias of itself")
	} else if !strings.Contains(err.Error(), "onto itself") {
		t.Errorf("err = %v, want the self-copy refusal", err)
	}
	got, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("read notes after the refused copy: %v", readErr)
	}
	if string(got) != body {
		t.Fatalf("notes.md = %q after copying the repo onto an alias of itself, want %q", got, body)
	}
}

// Two directories proven distinct can still hold hard links to one inode.
// SameDir answers a question about directories and cannot see that
// src/notes.md and dst/notes.md are one file, so opening the destination with
// O_TRUNC empties the source and only then begins reading it — the copy then
// reports success having written a repository full of nothing.
func TestCopyTreeWithoutGitRefusesAHardLinkedFile(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	const body = "keep me"
	notes := filepath.Join(src, "notes.md")
	if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	dst := filepath.Join(base, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Link(notes, filepath.Join(dst, "notes.md")); err != nil {
		t.Skipf("hard links unavailable in this environment: %v", err)
	}

	err := copyTreeWithoutGit(src, dst)
	if err == nil {
		t.Error("copied a file onto a hard link of itself")
	} else if !strings.Contains(err.Error(), "same file") {
		t.Errorf("err = %v, want the same-file refusal", err)
	}
	got, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("read notes after the refused copy: %v", readErr)
	}
	if string(got) != body {
		t.Fatalf("notes.md = %q after copying onto a hard link of itself, want %q", got, body)
	}
}

// A destination inside the source is not the source, so no identity comparison
// rejects it — but creating it adds an entry to the tree about to be walked,
// and the walk copies it into itself until a path length or recursion limit
// stops it. The refusal therefore has to come before the destination exists.
func TestCopyTreeWithoutGitRefusesADestinationInsideTheSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	const body = "keep me"
	notes := filepath.Join(src, "notes.md")
	if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	err := copyTreeWithoutGit(src, filepath.Join(src, "nested", "dst"))
	if err == nil {
		t.Fatal("copied a project memory repo into itself")
	}
	if !strings.Contains(err.Error(), "into itself") {
		t.Errorf("err = %v, want the copy-into-itself refusal", err)
	}
	// Refused before the destination was created, not after.
	if _, statErr := os.Stat(filepath.Join(src, "nested")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the destination was created inside the source before the refusal: %v", statErr)
	}
	got, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("read notes after the refused copy: %v", readErr)
	}
	if string(got) != body {
		t.Errorf("notes.md = %q, want %q", got, body)
	}
}

// The name-based containment check proves something about names, and the
// handles are opened afterwards. Re-pointing the destination into the source in
// that window defeats it on its own: the early check passed against a
// destination outside the source, MkdirAll then creates the entry inside the
// source, and the two pinned directories are still genuinely different — so no
// identity comparison objects and the walk copies the source into itself.
//
// The hook stages exactly that re-point, so the reproduction does not depend on
// winning a race.
func TestCopyTreeWithoutGitRefusesADestinationRepointedIntoTheSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	const body = "keep me"
	notes := filepath.Join(src, "notes.md")
	if err := os.WriteFile(notes, []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, outside, alias)

	swapped := false
	err := copyTreeWithoutGitHooked(src, filepath.Join(alias, "nested"), func() {
		if swapped {
			return
		}
		swapped = true
		if rmErr := os.Remove(alias); rmErr != nil {
			t.Fatalf("Remove alias: %v", rmErr)
		}
		mustLinkDir(t, src, alias)
	})
	if !swapped {
		t.Fatal("the hook never ran; the window was not exercised")
	}
	if err == nil {
		t.Fatal("copied a project memory repo into a destination re-pointed inside it")
	}
	if !strings.Contains(err.Error(), "into itself") {
		t.Errorf("err = %v, want the copy-into-itself refusal", err)
	}
	// The tell-tale of the recursion this prevents.
	if _, statErr := os.Stat(filepath.Join(src, "nested", "nested")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the copy recursed into its own destination: %v", statErr)
	}
	got, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("read notes after the refused copy: %v", readErr)
	}
	if string(got) != body {
		t.Errorf("notes.md = %q, want %q", got, body)
	}
}

// The traversal never descends into the destination, whoever moves it where.
// The containment checks answer where the destination was before the walk
// began; this answers for the directory actually in hand.
func TestCopyTreeBetweenRootsRefusesToDescendIntoTheDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(filepath.Join(src, "dst"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const body = "keep me"
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	srcRoot, _, err := rootfs.OpenIdentified(src)
	if err != nil {
		t.Fatalf("OpenIdentified(src): %v", err)
	}
	defer srcRoot.Close() //nolint:errcheck // test cleanup
	// Pinned directly, so the traversal meets its own destination as an entry
	// without any containment check having had the chance to object.
	dstRoot, _, err := rootfs.OpenIdentified(filepath.Join(src, "dst"))
	if err != nil {
		t.Fatalf("OpenIdentified(dst): %v", err)
	}
	defer dstRoot.Close() //nolint:errcheck // test cleanup

	err = (&repoCopy{dstTop: dstRoot}).copyDir(srcRoot, dstRoot, ".", 0)
	if err == nil {
		t.Fatal("the traversal descended into its own destination")
	}
	if !strings.Contains(err.Error(), "its own destination") {
		t.Errorf("err = %v, want the destination-entry refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(src, "dst", "dst")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the copy recursed into its own destination: %v", statErr)
	}
}

// Clearing a subdirectory by name and then re-resolving the same name to
// descend is two resolutions of one name. In the window between them the
// cleared directory can be renamed aside and another moved into its place, so
// the check passes on one directory and the walk reads another — and when the
// impostor is the destination, the walk copies it into itself.
//
// Pinning the child and descending through that handle removes the second
// resolution. The hook fires in exactly the window that used to exist.
//
// The impostor here is an ordinary directory rather than the destination
// itself, because the property under test is that the walk stays with what it
// cleared; staging it with the destination would additionally require renaming a
// directory the walk holds open, which Windows refuses.
func TestCopyDirStaysWithTheChildItCleared(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	child := filepath.Join(src, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const body = "from the original child"
	if err := os.WriteFile(filepath.Join(child, "original.txt"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	impostor := filepath.Join(base, "impostor")
	if err := os.MkdirAll(impostor, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(impostor, "impostor.txt"), []byte("swapped in"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	dst := filepath.Join(base, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	srcRoot, _, err := rootfs.OpenIdentified(src)
	if err != nil {
		t.Fatalf("OpenIdentified(src): %v", err)
	}
	defer srcRoot.Close() //nolint:errcheck // test cleanup
	dstRoot, _, err := rootfs.OpenIdentified(dst)
	if err != nil {
		t.Fatalf("OpenIdentified(dst): %v", err)
	}
	defer dstRoot.Close() //nolint:errcheck // test cleanup

	staged := false
	swapFailed := ""
	copier := &repoCopy{dstTop: dstRoot, afterChildCheck: func(rel string) {
		if staged || swapFailed != "" || rel != "child" {
			return
		}
		if rnErr := os.Rename(child, filepath.Join(src, "child-moved")); rnErr != nil {
			swapFailed = rnErr.Error()
			return
		}
		if rnErr := os.Rename(impostor, child); rnErr != nil {
			_ = os.Rename(filepath.Join(src, "child-moved"), child)
			swapFailed = rnErr.Error()
			return
		}
		staged = true
	}}

	err = copier.copyDir(srcRoot, dstRoot, ".", 0)
	if !staged {
		t.Fatalf("could not stage the swap this test exists to survive: %s", swapFailed)
	}
	if err != nil {
		t.Fatalf("copyDir after the swap: %v", err)
	}

	// The walk must have read the directory it cleared, wherever its name went.
	got, readErr := os.ReadFile(filepath.Join(dst, "child", "original.txt"))
	if readErr != nil {
		t.Fatalf("the copy did not follow the child it cleared: %v", readErr)
	}
	if string(got) != body {
		t.Errorf("copied content = %q, want %q", got, body)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "child", "impostor.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the walk read the directory that took over the name: %v", statErr)
	}
}

// The two trees must be disjoint. A source below the destination was once
// waved through on the reasoning that it cannot recurse; it can still lose
// data, because the walk writes a source subdirectory over the source itself.
func TestCopyTreeWithoutGitRefusesASourceInsideTheDestination(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "outer")
	src := filepath.Join(dst, "inner")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}

	err := copyTreeWithoutGit(src, dst)
	if err == nil {
		t.Fatal("copied a project memory repo into a directory that contains it")
	}
	if !strings.Contains(err.Error(), "which contains it") {
		t.Errorf("err = %v, want the containing-destination refusal", err)
	}
}

// The refusal must not cost the ordinary case: a genuine move still copies the
// whole working tree, including nested directories, and still leaves the source
// .git behind.
func TestCopyTreeWithoutGitCopiesNestedTree(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	nested := filepath.Join(src, "episodes", "coder")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "2026-01-01.md"), []byte("episode"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "source-only"), []byte("do not copy"), 0o644); err != nil {
		t.Fatalf("write git marker: %v", err)
	}

	dst := filepath.Join(base, "dst")
	if err := copyTreeWithoutGit(src, dst); err != nil {
		t.Fatalf("copyTreeWithoutGit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "episodes", "coder", "2026-01-01.md"))
	if err != nil {
		t.Fatalf("read nested copy: %v", err)
	}
	if string(got) != "episode" {
		t.Errorf("nested file = %q, want %q", got, "episode")
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "source-only")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("source .git was copied: %v", err)
	}
}

func TestMoveProjectRepoFailsClosedOnUnresolvablePath(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	if err := MoveProjectRepo(src, filepath.Join(base, "bad\x00name"), false); err == nil {
		t.Error("MoveProjectRepo proceeded with a destination it could not resolve")
	}
}
func TestMoveProjectRepoCopiesWorkingTreeWithoutGitDir(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo(src): %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".git", "source-only"), []byte("do not copy"), 0o644); err != nil {
		t.Fatalf("write git marker: %v", err)
	}

	dst := filepath.Join(tmp, "dst")
	if err := MoveProjectRepo(src, dst, false); err != nil {
		t.Fatalf("MoveProjectRepo: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "notes.md"))
	if err != nil {
		t.Fatalf("read copied notes: %v", err)
	}
	if string(got) != "keep me" {
		t.Fatalf("copied notes = %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "source-only")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source .git marker copied, err=%v", err)
	}
	if err := ValidateProjectRepo(dst, false); err != nil {
		t.Fatalf("ValidateProjectRepo(dst): %v", err)
	}
}
