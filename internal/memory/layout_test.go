package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestProjectLayout_ValidUserProject(t *testing.T) {
	got, err := ProjectLayout("my-project")
	if err != nil {
		t.Fatalf("ProjectLayout: %v", err)
	}

	wantPaths := map[string]bool{
		"projects/my-project":                 true,
		"projects/my-project/rules.md":        true,
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
		t.Error("ProjectLayout must not include queue.wal")
	}
}

func TestProjectLayout_Global(t *testing.T) {
	got, err := ProjectLayout("global")
	if err != nil {
		t.Fatalf("ProjectLayout: %v", err)
	}

	want := []LayoutItem{
		{Path: "projects/global", Dir: true, Desc: "System project (default scope)"},
		{Path: "projects/global/episodes", Dir: true, Desc: "Session episode files for the system project"},
		{Path: "projects/global/index", Dir: true, Desc: "Semantic search indexes for the system project"},
		{Path: "projects/global/index/_episodes", Dir: true, Desc: "Embeddings of the system project's episodes"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProjectLayout(global) =\n\t%v\nwant\n\t%v", got, want)
	}

	for _, item := range got {
		if item.Path == "projects/global/rules.md" {
			t.Error("ProjectLayout(global) must not include projects/global/rules.md")
		}
	}
}

func TestProjectLayout_InvalidSlug(t *testing.T) {
	tests := []string{"", "My Project", "foo_bar", "double--dash", " global "}
	for _, slug := range tests {
		t.Run(slug, func(t *testing.T) {
			if _, err := ProjectLayout(slug); err == nil {
				t.Errorf("ProjectLayout(%q): expected error, got nil", slug)
			}
		})
	}
}

func TestExpectedLayout_StableContent(t *testing.T) {
	got := ExpectedLayout()
	// Mirror the canonical layout from docs/architecture.md so a future
	// edit there forces a deliberate update here too. Project-scoped
	// runtime artifacts (sessions.jsonl, queue.wal, vectors.bin,
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

	found := false
	for _, item := range got {
		if item.Path == "global" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("MissingItems: expected wrong-kind 'global' to be flagged, got %v", got)
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
