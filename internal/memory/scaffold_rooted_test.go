package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// Scaffolding creates .gitkeep inside every layout directory. Before the
// migration it did so by joining the directory's *absolute* path and creating
// there, so a layout directory that was really a link put the file wherever the
// link led. Addressing it relative to the pinned repo refuses that.
func TestCreateMissing_LinkedLayoutDirectoryDoesNotPlaceGitkeepOutside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	// "episodes" already exists, as a link to a directory outside the repo.
	mustLinkDir(t, outside, filepath.Join(root, "episodes"))

	// The error is not the assertion — an existing directory of the right kind
	// is a legitimate skip. What matters is where the file did or did not land.
	_ = CreateMissingProjectRepo(root, false)

	if _, err := os.Stat(filepath.Join(outside, ".gitkeep")); err == nil {
		t.Fatal("scaffolding created .gitkeep outside the repository, through a linked layout directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("scaffolding wrote %d entries outside the repository: %v", len(entries), entries)
	}
}

// Scaffolding must also refuse to create a *file* item through a linked
// directory in the middle of its path.
func TestCreateMissing_LinkedParentDoesNotPlaceFilesOutside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	mustLinkDir(t, outside, filepath.Join(root, "agents"))

	_ = CreateMissing(root, []LayoutItem{{Path: "agents/coder/persona.md", Dir: false}})

	if _, err := os.Stat(filepath.Join(outside, "coder")); err == nil {
		t.Fatal("scaffolding created a directory outside the repository through a linked parent")
	}
}

// A repo path that is a file, not a directory, still reports the message the
// status page shows rather than a raw resolution error.
func TestOpenRepoRoot_FileReportsNotADirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := MissingProjectRepoItems(path, false)
	if err == nil {
		t.Fatal("expected an error for a repo path that is a file")
	}
	if got := err.Error(); got != "memory: repo path is not a directory: "+path {
		t.Fatalf("err = %q, want the not-a-directory message", got)
	}
}

// Validation pins the repo once and checks .git and the layout through that one
// handle. A repo whose .git is a link to a directory outside must not validate
// as a plain git repo on the strength of what the link points at.
func TestValidateProjectRepo_LinkedGitDirectoryIsNotAPlainRepo(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outsideGit := filepath.Join(base, "elsewhere", ".git")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(outsideGit, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustLinkDir(t, outsideGit, filepath.Join(root, ".git"))

	// A junction is refused by the root outright; a symlink to a directory
	// outside is refused too. Either way validation must not pass.
	if err := ValidateProjectRepo(root, false); err == nil {
		t.Fatal("a repo whose .git leads outside validated as a plain git repo")
	}
}
