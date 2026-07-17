package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/vrnc/harness/internal/project"
)

func TestProjectLayout_ValidUserProject(t *testing.T) {
	got, err := ProjectLayout("my-project")
	if err != nil {
		t.Fatalf("ProjectLayout: %v", err)
	}

	wantPaths := map[string]bool{
		"projects/my-project":                 true,
		"projects/my-project/rules.md":        true,
		"projects/my-project/user.md":         true,
		"projects/my-project/facts.md":        true,
		"projects/my-project/agents":          true,
		"projects/my-project/sessions.jsonl":  true,
		"projects/my-project/episodes":        true,
		"projects/my-project/index":           true,
		"projects/my-project/index/_episodes": true,
	}

	gotPaths := make(map[string]bool)
	for _, item := range got {
		gotPaths[item.Path] = true
	}

	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("ProjectLayout paths = %v, want %v", gotPaths, wantPaths)
	}

	if gotPaths["projects/my-project/queue.wal"] {
		t.Error("ProjectLayout must not include legacy queue.wal")
	}
}

func TestProjectLayout_Global(t *testing.T) {
	got, err := ProjectLayout("global")
	if err != nil {
		t.Fatalf("ProjectLayout: %v", err)
	}

	want := []LayoutItem{
		{Path: "projects/global", Dir: true, Desc: "System project (default scope)"},
		{Path: "projects/global/rules.md", Dir: false, Desc: "Project rules"},
		{Path: "projects/global/user.md", Dir: false, Desc: "Facts about the user for this project"},
		{Path: "projects/global/facts.md", Dir: false, Desc: "Promoted facts for this project"},
		{Path: "projects/global/agents", Dir: true, Desc: "Project agent definitions"},
		{Path: "projects/global/sessions.jsonl", Dir: false, Desc: "Project session history"},
		{Path: "projects/global/episodes", Dir: true, Desc: "Project episode files"},
		{Path: "projects/global/index", Dir: true, Desc: "Project semantic search indexes"},
		{Path: "projects/global/index/_episodes", Dir: true, Desc: "Project episode embeddings"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectLayout(global) =\n\t%v\nwant\n\t%v", got, want)
	}

}

func TestProjectLayout_InvalidSlug(t *testing.T) {
	tests := []string{"", "My Project", "foo_bar", "double--dash", " global "}
	for _, slug := range tests {
		t.Run(slug, func(t *testing.T) {
			if _, err := ProjectLayout(slug); !errors.Is(err, project.ErrInvalidSlug) {
				t.Errorf("ProjectLayout(%q): errors.Is(ErrInvalidSlug)=false, err=%v", slug, err)
			}
		})
	}
}

func TestExpectedLayout_StableContent(t *testing.T) {
	got := ExpectedLayout()
	// Mirror the canonical layout from docs/architecture.md so a future
	// edit there forces a deliberate update here too. Project-scoped
	// runtime artifacts (sessions.jsonl, vectors.bin,
	// manifest.json) are intentionally absent: they are owned by other
	// subsystems and live under projects/global/.
	want := []LayoutItem{
		{Path: "global", Dir: true, Desc: "Global prompt content"},
		{Path: "global/rules.md", Dir: false, Desc: "Always-on base prompt"},
		{Path: "global/user.md", Dir: false, Desc: "Hand-authored facts about the user"},
		{Path: "global/facts.md", Dir: false, Desc: "Promoted cross-agent facts"},
		{Path: "agents", Dir: true, Desc: "Global agents library (definition only)"},
		{Path: "projects", Dir: true, Desc: "Per-project session/episode/queue/index data"},
		{Path: "projects/global", Dir: true, Desc: "System project (default scope)"},
		{Path: "projects/global/episodes", Dir: true, Desc: "Session episode files for the system project"},
		{Path: "projects/global/index", Dir: true, Desc: "Semantic search indexes for the system project"},
		{Path: "projects/global/index/_episodes", Dir: true, Desc: "Embeddings of the system project's episodes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExpectedLayout =\n\t%v\nwant\n\t%v", got, want)
	}
}

func TestMissingItems_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	got, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems: %v", err)
	}
	if len(got) != len(ExpectedLayout()) {
		t.Errorf("MissingItems on empty root: got %d, want %d", len(got), len(ExpectedLayout()))
	}
}

func TestMissingItems_FullRoot(t *testing.T) {
	root := t.TempDir()
	for _, item := range ExpectedLayout() {
		abs := filepath.Join(root, filepath.FromSlash(item.Path))
		if item.Dir {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", abs, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte("seed"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", abs, err)
		}
	}
	got, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("MissingItems on full root: got %v, want []", got)
	}
}

func TestMissingItems_PartialRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "global"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "global", "rules.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems: %v", err)
	}
	gotPaths := make([]string, 0, len(got))
	for _, item := range got {
		gotPaths = append(gotPaths, item.Path)
	}
	sort.Strings(gotPaths)
	want := []string{
		"agents",
		"global/facts.md",
		"global/user.md",
		"projects",
		"projects/global",
		"projects/global/episodes",
		"projects/global/index",
		"projects/global/index/_episodes",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(gotPaths, want) {
		t.Errorf("MissingItems partial: got %v, want %v", gotPaths, want)
	}
}

func TestMissingItems_WrongKind(t *testing.T) {
	root := t.TempDir()
	// Place a file where a directory is expected.
	if err := os.WriteFile(filepath.Join(root, "global"), []byte("oops"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems: %v", err)
	}

	foundGlobal := false
	foundChild := false
	for _, item := range got {
		if item.Path == "global" {
			foundGlobal = true
		}
		if item.Path == "global/rules.md" {
			foundChild = true
		}
	}
	if !foundGlobal {
		t.Errorf("MissingItems: expected wrong-kind 'global' to be flagged, got %v", got)
	}
	if !foundChild {
		t.Errorf("MissingItems: expected child of wrong-kind 'global' to be flagged, got %v", got)
	}
}

func TestMissingItems_EmptyPath(t *testing.T) {
	if _, err := MissingItems(""); err == nil {
		t.Error("MissingItems(\"\"): expected error, got nil")
	}
}

func TestMissingItems_NonexistentRoot(t *testing.T) {
	if _, err := MissingItems(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("MissingItems on missing root: expected error, got nil")
	}
}

func TestMissingItems_RootIsFile(t *testing.T) {
	root := t.TempDir()
	bogus := filepath.Join(root, "file.txt")
	if err := os.WriteFile(bogus, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := MissingItems(bogus); err == nil {
		t.Error("MissingItems on file path: expected error, got nil")
	}
}

func TestValidateRepo_OK(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	if err := CreateMissing(root, ExpectedLayout()); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	if err := ValidateRepo(root); err != nil {
		t.Fatalf("ValidateRepo: %v", err)
	}
}

func TestValidateRepo_RequiresGitRepo(t *testing.T) {
	root := t.TempDir()
	if err := CreateMissing(root, ExpectedLayout()); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	if err := ValidateRepo(root); err == nil {
		t.Fatal("ValidateRepo without .git: expected error, got nil")
	}
}

func TestValidateRepo_RejectsIncompleteLayout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "global"), 0o755); err != nil {
		t.Fatalf("MkdirAll global: %v", err)
	}
	if err := ValidateRepo(root); err == nil {
		t.Fatal("ValidateRepo on incomplete layout: expected error, got nil")
	}
}

func TestValidateRepo_RejectsGitFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: elsewhere"), 0o644); err != nil {
		t.Fatalf("WriteFile .git: %v", err)
	}
	if err := CreateMissing(root, ExpectedLayout()); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}
	if err := ValidateRepo(root); err == nil {
		t.Fatal("ValidateRepo with .git file: expected error, got nil")
	}
}

func TestCreateMissing_All(t *testing.T) {
	root := t.TempDir()
	if err := CreateMissing(root, ExpectedLayout()); err != nil {
		t.Fatalf("CreateMissing: %v", err)
	}

	// Re-check: after scaffolding everything, MissingItems must be empty.
	missing, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems after scaffold: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("MissingItems after scaffold: got %v, want []", missing)
	}

	// File items must be created as zero-byte files; directory items
	// must be directories.
	for _, item := range ExpectedLayout() {
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
	if err := os.MkdirAll(filepath.Join(root, "global"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rules := filepath.Join(root, "global", "rules.md")
	if err := os.WriteFile(rules, []byte("user wrote this"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := CreateMissing(root, ExpectedLayout()); err != nil {
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
	bogus := filepath.Join(root, "global")
	if err := os.WriteFile(bogus, []byte("user data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	missing, err := MissingItems(root)
	if err != nil {
		t.Fatalf("MissingItems: %v", err)
	}
	// CreateMissing may surface an error here because children of
	// global/ cannot be created while global/ is a file; the contract
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
	if err := CreateMissing("", ExpectedLayout()); err == nil {
		t.Error("CreateMissing(\"\"): expected error, got nil")
	}
}

func TestCreateMissing_NonexistentRoot(t *testing.T) {
	if err := CreateMissing(filepath.Join(t.TempDir(), "does-not-exist"), ExpectedLayout()); err == nil {
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
