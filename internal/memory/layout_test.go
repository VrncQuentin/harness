package memory

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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

func TestSameProjectRepoPathUsesOSPathIdentity(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "Repo")
	b := filepath.Join(base, "repo")
	got := SameProjectRepoPath(a, b)
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
