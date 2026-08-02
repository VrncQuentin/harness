package memory

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/pathid"
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

// The scaffolder addresses layout entries by validated repo-relative paths
// through the pinned root, never by joining an absolute layout-directory path
// and reopening it. The cases here are the ones where identity selection
// changes: an ordinary directory (different), a root reached through an alias
// (same physical repository), a layout directory that is itself a link leaving
// the repo (alias — must not place .gitkeep outside), and a root alias
// repointed after the pin (repoint — must fail closed).
func TestCreateMissing_LinkedLayoutDirectoryDoesNotPlaceGitkeepOutside(t *testing.T) {
	t.Run("ordinary layout directory", func(t *testing.T) {
		root := t.TempDir()
		if err := CreateMissing(root, []LayoutItem{{Path: "artifacts", Dir: true}}); err != nil {
			t.Fatalf("CreateMissing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "artifacts", ".gitkeep")); err != nil {
			t.Fatalf(".gitkeep was not created inside the ordinary layout directory: %v", err)
		}
	})

	t.Run("root reached through an alias spelling", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		alias := filepath.Join(base, "alias")
		mustLinkDir(t, real, alias)
		if err := CreateMissing(alias, []LayoutItem{{Path: "artifacts", Dir: true}}); err != nil {
			t.Fatalf("CreateMissing through an alias spelling: %v", err)
		}
		// The scaffold lands in the physical root, not a copy at the alias.
		if _, err := os.Stat(filepath.Join(real, "artifacts", ".gitkeep")); err != nil {
			t.Fatalf(".gitkeep missing from the physical root reached through the alias: %v", err)
		}
	})

	t.Run("layout directory linked outside", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "repo")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		outside := filepath.Join(base, "outside")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		link := filepath.Join(root, "artifacts")
		mustLinkDir(t, outside, link)

		if err := CreateMissing(root, []LayoutItem{{Path: "artifacts", Dir: true}}); err == nil {
			t.Error("CreateMissing succeeded with a layout directory linked outside the repo")
		}
		if _, statErr := os.Stat(filepath.Join(outside, ".gitkeep")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf(".gitkeep escaped the repo through the linked layout directory: %v", statErr)
		}
	})

	t.Run("root alias repointed after pin", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		alias := filepath.Join(base, "alias")
		mustLinkDir(t, real, alias)
		replacement := filepath.Join(base, "replacement")
		if err := os.MkdirAll(replacement, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		swapped := false
		err := createMissingHooked(alias, []LayoutItem{{Path: "artifacts", Dir: true}}, func() {
			if swapped {
				return
			}
			swapped = true
			if rmErr := os.Remove(alias); rmErr != nil {
				t.Fatalf("Remove alias: %v", rmErr)
			}
			mustLinkDir(t, replacement, alias)
		})
		if !swapped {
			t.Fatal("the hook never ran; the window was not exercised")
		}
		if err == nil {
			t.Error("CreateMissing succeeded on a root alias repointed after the pin")
		}
		// The replacement must not have received the scaffold.
		if _, statErr := os.Stat(filepath.Join(replacement, "artifacts")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the replacement received the scaffold: %v", statErr)
		}
	})
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
func TestMoveProjectRepo_SameRepoAlias(t *testing.T) {
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
func TestMoveProjectRepo_RecursiveSelfCopy(t *testing.T) {
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

// A destination entry hard-linked to a source file must not be written through.
// Publishing by rename replaces the entry and leaves the inode alone, so the
// source keeps its content and the copy still succeeds — writing through the
// link instead would empty the source file.
func TestCopyTreeWithoutGitPreservesAHardLinkedSourceFile(t *testing.T) {
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

	if err := copyTreeWithoutGit(src, dst); err != nil {
		t.Fatalf("copyTreeWithoutGit: %v", err)
	}
	got, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("read the source after the copy: %v", readErr)
	}
	if string(got) != body {
		t.Fatalf("source notes.md = %q, want %q — the copy wrote through the link", got, body)
	}
	copied, readErr := os.ReadFile(filepath.Join(dst, "notes.md"))
	if readErr != nil {
		t.Fatalf("read the destination after the copy: %v", readErr)
	}
	if string(copied) != body {
		t.Errorf("destination notes.md = %q, want %q", copied, body)
	}
}

// The case comparing the pair being copied cannot see: the destination entry is
// a hard link to a *different* source file. Two different inodes, so a
// source-versus-destination comparison passes, and truncating the destination
// empties a source file the copy has not reached yet.
func TestCopyTreeWithoutGitPreservesACrossLinkedSourceFile(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	const bodyA, bodyB = "A", "B"
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte(bodyA), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	fileB := filepath.Join(src, "b.txt")
	if err := os.WriteFile(fileB, []byte(bodyB), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	dst := filepath.Join(base, "dst")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// dst/a.txt is b.txt under another name.
	if err := os.Link(fileB, filepath.Join(dst, "a.txt")); err != nil {
		t.Skipf("hard links unavailable in this environment: %v", err)
	}

	if err := copyTreeWithoutGit(src, dst); err != nil {
		t.Fatalf("copyTreeWithoutGit: %v", err)
	}
	got, readErr := os.ReadFile(fileB)
	if readErr != nil {
		t.Fatalf("read b.txt after the copy: %v", readErr)
	}
	if string(got) != bodyB {
		t.Fatalf("a different source file was overwritten through a destination hard link: b.txt = %q, want %q", got, bodyB)
	}
	copied, readErr := os.ReadFile(filepath.Join(dst, "a.txt"))
	if readErr != nil {
		t.Fatalf("read the destination after the copy: %v", readErr)
	}
	if string(copied) != bodyA {
		t.Errorf("destination a.txt = %q, want %q", copied, bodyA)
	}
}

// A destination inside the source is not the source, so no identity comparison
// rejects it — but creating it adds an entry to the tree about to be walked,
// and the walk copies it into itself until a path length or recursion limit
// stops it. The refusal therefore has to come before the destination exists.
func TestMoveProjectRepo_NestedDestination(t *testing.T) {
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
func TestMoveProjectRepo_ContainmentCheckByNameIsInsufficient(t *testing.T) {
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

	err = (&repoCopy{srcStack: []*rootfs.Root{srcRoot}, dstStack: []*rootfs.Root{dstRoot}}).copyDir(".")
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
func TestMoveProjectRepo_DescendsThroughPinnedChild(t *testing.T) {
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
	copier := &repoCopy{srcStack: []*rootfs.Root{srcRoot}, dstStack: []*rootfs.Root{dstRoot}, afterChildCheck: func(rel string) {
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

	err = copier.copyDir(".")
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

// anySameDir is what keeps the two trees disjoint at every level, so it has to
// find a match anywhere in the stack, not just at the end. Two handles opened on
// one directory are the same directory, which needs no links to arrange and so
// exercises the comparison on every platform.
func TestAnySameDir(t *testing.T) {
	base := t.TempDir()
	names := []string{"top", "middle", "bottom"}
	roots := make([]*rootfs.Root, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		root, err := rootfs.Open(dir)
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		defer root.Close() //nolint:errcheck // test cleanup
		roots = append(roots, root)
	}

	for i, name := range names {
		candidate, err := rootfs.Open(filepath.Join(base, name))
		if err != nil {
			t.Fatalf("Open(%s): %v", name, err)
		}
		same, err := anySameDir(candidate, roots)
		_ = candidate.Close()
		if err != nil {
			t.Fatalf("anySameDir: %v", err)
		}
		if !same {
			t.Errorf("anySameDir missed a match at stack position %d (%s)", i, name)
		}
	}

	unrelated := filepath.Join(base, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	candidate, err := rootfs.Open(unrelated)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer candidate.Close() //nolint:errcheck // test cleanup
	same, err := anySameDir(candidate, roots)
	if err != nil {
		t.Fatalf("anySameDir: %v", err)
	}
	if same {
		t.Error("anySameDir matched a directory that is in neither tree")
	}
}

// The destination side needs the same protection as the source side. The source
// child is pinned safely, but the destination counterpart is opened afterwards,
// and in that window a pinned *source ancestor* can be moved into the name the
// destination is about to be opened under. The copy then writes a subdirectory
// over its own parent, and the per-file check cannot object: those are different
// files at different relative paths.
//
// Every newly pinned destination directory is therefore checked against every
// pinned source directory, which is what catches it.
func TestMoveProjectRepo_RenamedChildMidCopy(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	parent := filepath.Join(src, "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const parentBody, childBody = "parent content", "child content"
	if err := os.WriteFile(filepath.Join(parent, "keep.txt"), []byte(parentBody), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(child, "keep.txt"), []byte(childBody), 0o644); err != nil {
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
	copier := &repoCopy{
		srcStack: []*rootfs.Root{srcRoot},
		dstStack: []*rootfs.Root{dstRoot},
		afterChildCheck: func(rel string) {
			if staged || swapFailed != "" || rel != filepath.Join("parent", "child") {
				return
			}
			// Move the pinned source parent under the name the destination
			// child is about to be opened as.
			if rnErr := os.Rename(parent, filepath.Join(dst, "parent", "child")); rnErr != nil {
				swapFailed = rnErr.Error()
				return
			}
			staged = true
		},
	}

	err = copier.copyDir(".")
	if !staged {
		if runtime.GOOS != "windows" {
			t.Fatalf("could not stage the ancestor move this test exists to survive: %s", swapFailed)
		}
		t.Skipf("cannot move a pinned ancestor here: %s", swapFailed)
	}
	if err == nil {
		t.Fatal("the copy accepted a destination that had become a source ancestor")
	}
	if !strings.Contains(err.Error(), "its own source") {
		t.Errorf("err = %v, want the destination-in-source refusal", err)
	}
	// The parent's own file must not have been overwritten by the child's.
	got, readErr := os.ReadFile(filepath.Join(dst, "parent", "child", "keep.txt"))
	if readErr != nil {
		t.Fatalf("read the moved parent's file: %v", readErr)
	}
	if string(got) != parentBody {
		t.Errorf("the copy wrote a subdirectory over its own parent: keep.txt = %q, want %q", got, parentBody)
	}
}

// The two trees must be disjoint in both directions. A source below the
// destination was once waved through on the reasoning that it cannot recurse;
// it can still lose data, because the walk writes a source subdirectory over
// the source itself. The reverse — a destination inside the source — recurses.
// Either way the move is refused before anything is created.
func TestMoveProjectRepo_OverlappingTreesRejected(t *testing.T) {
	tests := []struct {
		name  string
		setup func(base string) (src, dst string)
		want  string
	}{
		{"source inside destination", func(base string) (string, string) {
			dst := filepath.Join(base, "outer")
			return filepath.Join(dst, "inner"), dst
		}, "which contains it"},
		{"destination inside source", func(base string) (string, string) {
			src := filepath.Join(base, "src")
			return src, filepath.Join(src, "nested", "dst")
		}, "into itself"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			src, dst := tc.setup(base)
			if err := EnsureProjectRepo(src, false); err != nil {
				t.Fatalf("EnsureProjectRepo: %v", err)
			}
			err := copyTreeWithoutGit(src, dst)
			if err == nil {
				t.Fatalf("copied a project memory repo into an overlapping destination %s", dst)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want the %s refusal", err, tc.want)
			}
		})
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

// The whole working tree is copied through the two pinned ends of the move,
// including nested content, while the source .git stays behind. A complete
// copy is part of the discriminator; the rest proves the two ends are pinned:
// an alias repointed after both roots are pinned must not redirect the copy.
// The former pathname-based implementation re-resolved each file by name, so
// it would read the repointed source and write the repointed destination.
func TestMoveProjectRepo_SourceAndDestinationPinned(t *testing.T) {
	t.Run("complete copy", func(t *testing.T) {
		tmp := t.TempDir()
		src := filepath.Join(tmp, "src")
		if err := EnsureProjectRepo(src, false); err != nil {
			t.Fatalf("EnsureProjectRepo(src): %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
			t.Fatalf("write notes: %v", err)
		}
		nested := filepath.Join(src, "episodes", "coder")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(nested, "2026-01-01.md"), []byte("episode"), 0o644); err != nil {
			t.Fatalf("write nested episode: %v", err)
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
		if got, err := os.ReadFile(filepath.Join(dst, "episodes", "coder", "2026-01-01.md")); err != nil {
			t.Fatalf("read nested copy: %v", err)
		} else if string(got) != "episode" {
			t.Fatalf("nested file = %q, want %q", string(got), "episode")
		}
		if _, err := os.Stat(filepath.Join(dst, ".git", "source-only")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source .git marker copied, err=%v", err)
		}
		if err := ValidateProjectRepo(dst, false); err != nil {
			t.Fatalf("ValidateProjectRepo(dst): %v", err)
		}
	})

	t.Run("source alias repointed mid-copy", func(t *testing.T) {
		base := t.TempDir()
		realSrc := filepath.Join(base, "real-src")
		if err := EnsureProjectRepo(realSrc, false); err != nil {
			t.Fatalf("EnsureProjectRepo(realSrc): %v", err)
		}
		if err := os.WriteFile(filepath.Join(realSrc, "notes.md"), []byte("from the pinned source"), 0o644); err != nil {
			t.Fatalf("write real source notes: %v", err)
		}
		impostor := filepath.Join(base, "impostor")
		if err := EnsureProjectRepo(impostor, false); err != nil {
			t.Fatalf("EnsureProjectRepo(impostor): %v", err)
		}
		if err := os.WriteFile(filepath.Join(impostor, "notes.md"), []byte("from the impostor"), 0o644); err != nil {
			t.Fatalf("write impostor notes: %v", err)
		}
		if err := os.WriteFile(filepath.Join(impostor, "marker.txt"), []byte("do not copy"), 0o644); err != nil {
			t.Fatalf("write impostor marker: %v", err)
		}
		alias := filepath.Join(base, "src-alias")
		mustLinkDir(t, realSrc, alias)
		dst := filepath.Join(base, "dst")

		swapped := false
		err := moveProjectRepoHooked(alias, dst, false, nil, func() {
			if swapped {
				return
			}
			swapped = true
			if rmErr := os.Remove(alias); rmErr != nil {
				t.Fatalf("Remove alias: %v", rmErr)
			}
			mustLinkDir(t, impostor, alias)
		})
		if !swapped {
			t.Fatal("the hook never ran; the window was not exercised")
		}
		if err != nil {
			t.Fatalf("MoveProjectRepo: %v", err)
		}
		// The copy must have read the pinned source, wherever the alias went.
		got, readErr := os.ReadFile(filepath.Join(dst, "notes.md"))
		if readErr != nil {
			t.Fatalf("read copied notes: %v", readErr)
		}
		if string(got) != "from the pinned source" {
			t.Errorf("copied notes = %q, want the pinned source's content — the copy followed the repointed alias", string(got))
		}
		if _, statErr := os.Stat(filepath.Join(dst, "marker.txt")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the impostor was copied: %v", statErr)
		}
	})

	t.Run("destination alias repointed mid-copy", func(t *testing.T) {
		base := t.TempDir()
		src := filepath.Join(base, "src")
		if err := EnsureProjectRepo(src, false); err != nil {
			t.Fatalf("EnsureProjectRepo(src): %v", err)
		}
		if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
			t.Fatalf("write notes: %v", err)
		}
		realDst := filepath.Join(base, "real-dst")
		if err := os.MkdirAll(realDst, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		impostor := filepath.Join(base, "impostor-dst")
		if err := os.MkdirAll(impostor, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		dstAlias := filepath.Join(base, "dst-alias")
		mustLinkDir(t, realDst, dstAlias)

		// Both trees are pinned before the alias is re-pointed. The copy must
		// write through the pinned destination handle, wherever the alias now
		// resolves.
		dstRoot, dstID, err := rootfs.OpenIdentified(dstAlias)
		if err != nil {
			t.Fatalf("OpenIdentified(dstAlias): %v", err)
		}
		defer dstRoot.Close() //nolint:errcheck // test cleanup
		srcRoot, srcID, err := rootfs.OpenIdentified(src)
		if err != nil {
			t.Fatalf("OpenIdentified(src): %v", err)
		}
		defer srcRoot.Close() //nolint:errcheck // test cleanup

		if rmErr := os.Remove(dstAlias); rmErr != nil {
			t.Fatalf("Remove dstAlias: %v", rmErr)
		}
		mustLinkDir(t, impostor, dstAlias)

		if err := copyTreeBetweenRoots(srcRoot, srcID, dstRoot, dstID, src, dstAlias); err != nil {
			t.Fatalf("copyTreeBetweenRoots: %v", err)
		}
		// The copy must have written to the pinned destination, wherever the
		// alias went.
		got, readErr := os.ReadFile(filepath.Join(realDst, "notes.md"))
		if readErr != nil {
			t.Fatalf("read copied notes: %v", readErr)
		}
		if string(got) != "keep me" {
			t.Errorf("copied notes = %q, want %q", string(got), "keep me")
		}
		if _, statErr := os.Stat(filepath.Join(impostor, "notes.md")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("the copy followed the repointed destination alias: %v", statErr)
		}
	})
}

// The transaction gate in EnsureProjectRepo belongs to the directory git
// opened. A directory replaced at the same pathname between that open and the
// scaffold open canonicalizes to the same pathid key — an identity comparison
// would accept it — while the retained-boundary comparison must reject it.
// This is the exact-boundary discriminator, analogous to the runtime's
// active-reader↔git check.
func TestEnsureProjectRepo_BoundaryMismatchFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := EnsureProjectRepo(root, false); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	origID, err := pathid.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the directory at the same pathname between the git open and the
	// scaffold pin.
	swapped := false
	swapFailed := ""
	err = ensureProjectRepoHooked(root, false, func() {
		if swapped || swapFailed != "" {
			return
		}
		if rnErr := os.Rename(root, root+".aside"); rnErr != nil {
			swapFailed = rnErr.Error()
			return
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			swapFailed = err.Error()
			return
		}
		swapped = true
	})
	if !swapped {
		if swapFailed != "" {
			t.Skipf("Windows: the retained boundary handle blocks replacing the directory at the same pathname; the handle-block is the fail-closed outcome: %s", swapFailed)
		}
		t.Fatal("the hook never ran; the window was not exercised")
	}
	if err == nil {
		t.Fatal("EnsureProjectRepo scaffolded under a boundary different from the git repository")
	}
	// The replacement must not have received the scaffold.
	if _, statErr := os.Stat(filepath.Join(root, "rules.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the replacement received the scaffold: %v", statErr)
	}
	// Precondition: the same-name replacement reuses the pathid key, so the
	// rejection cannot be explained by a pathid-based comparison.
	replacedID, err := pathid.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if !origID.Equal(replacedID) {
		t.Fatal("same-name replacement must reuse the pathid key, or this test no longer discriminates")
	}
}

// The MoveProjectRepo boundary comparison must fail when the destination is
// re-pointed between the pinned destination handle and git opening the
// destination: the pinned handle refers to one physical repository while the
// git boundary and its coordinator refer to another. The copy and scaffold
// must never run.
//
// The staging re-points an intermediate component (the junction alias) so it
// works on every platform, and the same-name-replacement precondition is not
// needed because the two destinations never shared a pathname.
func TestMoveProjectRepo_BoundaryMismatchFailsClosed(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo(src): %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	parentA := filepath.Join(base, "parent-a")
	if err := os.MkdirAll(parentA, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	parentB := filepath.Join(base, "parent-b")
	if err := os.MkdirAll(parentB, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, parentA, alias)
	dst := filepath.Join(alias, "repo")

	swapped := false
	err := moveProjectRepoHooked(src, dst, false, nil, func() {
		if swapped {
			return
		}
		swapped = true
		if rmErr := os.RemoveAll(alias); rmErr != nil {
			t.Fatalf("Remove alias: %v", rmErr)
		}
		mustLinkDir(t, parentB, alias)
	})
	if !swapped {
		t.Fatal("the hook never ran; the window was not exercised")
	}
	if err == nil {
		t.Fatal("MoveProjectRepo moved into a destination different from the git boundary")
	}
	// Neither the repointed destination nor the pinned one may have received
	// the copy or scaffold.
	if _, statErr := os.Stat(filepath.Join(parentB, "repo", "notes.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the repointed destination received the copy: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(parentA, "repo", "notes.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the pinned destination received the copy: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(parentB, "repo", "rules.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the repointed destination received the scaffold: %v", statErr)
	}
}

// A destination re-pointed into the source after the name-based containment
// check must be refused by the handle-bound check before the destination is
// initialized as a git repository: no .git metadata and no copied files may
// appear inside the source. This is the race the pinning ordering exists to
// survive.
func TestMoveProjectRepo_RefusedRepointLeavesNoGitInSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo(src): %v", err)
	}
	const body = "keep me"
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, outside, alias)
	dst := filepath.Join(alias, "nested", "dst")

	swapped := false
	err := moveProjectRepoHooked(src, dst, false, func() {
		if swapped {
			return
		}
		swapped = true
		if rmErr := os.RemoveAll(alias); rmErr != nil {
			t.Fatalf("Remove alias: %v", rmErr)
		}
		mustLinkDir(t, src, alias)
	}, nil)
	if !swapped {
		t.Fatal("the hook never ran; the window was not exercised")
	}
	if err == nil {
		t.Fatal("MoveProjectRepo copied into a destination re-pointed inside the source")
	}
	// The refused move must not have initialized a repository inside the
	// source, and must not have copied anything there.
	if _, statErr := os.Stat(filepath.Join(src, "nested", "dst", ".git")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf(".git was created inside the source at the re-pointed destination: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(src, "nested", "dst", "notes.md")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("copied files were left at the re-pointed destination inside the source: %v", statErr)
	}
	// The source's own tree is intact.
	got, readErr := os.ReadFile(filepath.Join(src, "notes.md"))
	if readErr != nil {
		t.Fatalf("read source notes: %v", readErr)
	}
	if string(got) != body {
		t.Errorf("source notes = %q, want %q", string(got), body)
	}
}

// The destination may name any number of missing ancestor components; they are
// created through the pinned deepest existing ancestor rather than by name.
func TestCopyTreeWithoutGit_CreatesMissingAncestors(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	dst := filepath.Join(base, "missing", "nested", "dst")
	if err := copyTreeWithoutGit(src, dst); err != nil {
		t.Fatalf("copyTreeWithoutGit: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(base, "missing", "nested", "dst", "notes.md"))
	if readErr != nil {
		t.Fatalf("read copied notes: %v", readErr)
	}
	if string(got) != "keep me" {
		t.Errorf("copied notes = %q, want %q", string(got), "keep me")
	}
}

// A trailing separator must not turn the destination into a nested
// dst/dst directory: it is cleaned and the files land in dst itself.
func TestCopyTreeWithoutGit_TrailingSeparatorDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	dst := filepath.Join(base, "dst") + string(filepath.Separator)
	if err := copyTreeWithoutGit(src, dst); err != nil {
		t.Fatalf("copyTreeWithoutGit: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(base, "dst", "notes.md"))
	if readErr != nil {
		t.Fatalf("read copied notes: %v", readErr)
	}
	if string(got) != "keep me" {
		t.Errorf("copied notes = %q, want %q", string(got), "keep me")
	}
	if _, statErr := os.Stat(filepath.Join(base, "dst", "dst")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("trailing separator created a nested destination: %v", statErr)
	}
}

// MoveProjectRepo cleans its destination, so a trailing separator and missing
// ancestor components are accepted the same way they were by os.MkdirAll.
func TestMoveProjectRepo_TrailingSeparatorDestination(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := EnsureProjectRepo(src, false); err != nil {
		t.Fatalf("EnsureProjectRepo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	dst := filepath.Join(base, "missing", "dst") + string(filepath.Separator)
	if err := MoveProjectRepo(src, dst, false); err != nil {
		t.Fatalf("MoveProjectRepo: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(base, "missing", "dst", "notes.md"))
	if readErr != nil {
		t.Fatalf("read copied notes: %v", readErr)
	}
	if string(got) != "keep me" {
		t.Errorf("copied notes = %q, want %q", string(got), "keep me")
	}
	if _, statErr := os.Stat(filepath.Join(base, "missing", "dst", "dst")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("trailing separator created a nested destination: %v", statErr)
	}
}

// TestPinProjectRepoDetectsRepointedBoundary verifies the handle-bound proof
// behind project-edit identity decisions: SameAs confirms a path that still
// names the pinned physical repository (including an alias) and fails closed
// once the alias is repointed to a different repository.
func TestPinProjectRepoDetectsRepointedBoundary(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := EnsureProjectRepo(repo, false); err != nil {
		t.Fatal(err)
	}

	proof, err := PinProjectRepo(repo)
	if err != nil {
		t.Fatalf("PinProjectRepo: %v", err)
	}
	defer func() { _ = proof.Close() }()

	// A stable spelling still names the pinned repository.
	ok, err := proof.SameAs(repo)
	if err != nil || !ok {
		t.Fatalf("SameAs(repo) = %v, %v; want true", ok, err)
	}

	// An alias of the same physical repository still names it.
	base := t.TempDir()
	alias := filepath.Join(base, "alias")
	symlinkOrSkip(t, repo, alias)
	ok, err = proof.SameAs(alias)
	if err != nil || !ok {
		t.Fatalf("SameAs(alias) = %v, %v; want true", ok, err)
	}

	// Repointing the alias at a different repository fails closed.
	other := filepath.Join(t.TempDir(), "other")
	if err := EnsureProjectRepo(other, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(alias); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, other, alias)
	ok, err = proof.SameAs(alias)
	if err != nil {
		t.Fatalf("SameAs(repointed alias): %v", err)
	}
	if ok {
		t.Fatal("repointed alias still reported as the pinned repository")
	}

	// An unresolvable path fails closed, not as "same".
	bad := filepath.Join(base, "missing", "component")
	if _, err := proof.SameAs(bad); err == nil {
		t.Fatal("SameAs on an unresolvable path must fail closed with an error")
	}
}
