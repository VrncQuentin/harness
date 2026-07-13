package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLayoutV2ReaderMapsLogicalPathsToProjectRepos(t *testing.T) {
	globalRoot := t.TempDir()
	activeRoot := t.TempDir()
	if err := CreateMissingProjectRepo(globalRoot, true); err != nil {
		t.Fatalf("global scaffold: %v", err)
	}
	if err := CreateMissingProjectRepo(activeRoot, false); err != nil {
		t.Fatalf("active scaffold: %v", err)
	}
	write := func(root, rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(rel))), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(globalRoot, "rules.md", "global rules")
	write(globalRoot, "agents/coder/persona.md", "global persona")
	write(activeRoot, "rules.md", "project rules")
	write(activeRoot, "agents/coder/persona.md", "project persona")
	write(activeRoot, "episodes/coder/2026-01-01T00-00-00Z.md", "episode")

	r := NewLayoutV2Reader(globalRoot, "demo", activeRoot)
	cases := map[string]string{
		"global/rules.md":                                      "global rules",
		"agents/coder/persona.md":                              "global persona",
		"projects/demo/rules.md":                               "project rules",
		"projects/demo/agents/coder/persona.md":                "project persona",
		"projects/demo/episodes/coder/2026-01-01T00-00-00Z.md": "episode",
	}
	for logical, want := range cases {
		got, err := r.Read(logical)
		if err != nil {
			t.Fatalf("read %s: %v", logical, err)
		}
		if string(got) != want {
			t.Fatalf("read %s = %q, want %q", logical, got, want)
		}
	}

	matches, err := r.Glob("projects/demo/episodes/coder/*.md")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	wantMatches := []string{"projects/demo/episodes/coder/2026-01-01T00-00-00Z.md"}
	if !reflect.DeepEqual(matches, wantMatches) {
		t.Fatalf("matches = %#v, want %#v", matches, wantMatches)
	}

	if err := r.WriteFile("global/facts.md", []byte("fact")); err != nil {
		t.Fatalf("write global fact: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(globalRoot, "facts.md")); err != nil || string(got) != "fact" {
		t.Fatalf("global facts physical = %q, %v", got, err)
	}
	if err := r.WriteFile("projects/demo/episodes/coder/new.md", []byte("new")); err != nil {
		t.Fatalf("write episode: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(activeRoot, "episodes", "coder", "new.md")); err != nil || string(got) != "new" {
		t.Fatalf("active episode physical = %q, %v", got, err)
	}
}

type fakeCommitter struct {
	files []string
}

func (f *fakeCommitter) Commit(_ string, files []string) (string, error) {
	f.files = append([]string(nil), files...)
	return "sha", nil
}

func TestLayoutV2CommitterMapsFilesToPhysicalRepo(t *testing.T) {
	r := NewLayoutV2Reader(t.TempDir(), "demo", t.TempDir())
	global := &fakeCommitter{}
	active := &fakeCommitter{}
	c := &LayoutV2Committer{Reader: r, Global: global, Active: active}
	if _, err := c.Commit("msg", []string{"projects/demo/episodes/coder/id.md"}); err != nil {
		t.Fatalf("commit active: %v", err)
	}
	if !reflect.DeepEqual(active.files, []string{"episodes/coder/id.md"}) {
		t.Fatalf("active files = %#v", active.files)
	}
	if _, err := c.Commit("msg", []string{"global/facts.md"}); err != nil {
		t.Fatalf("commit global: %v", err)
	}
	if !reflect.DeepEqual(global.files, []string{"facts.md"}) {
		t.Fatalf("global files = %#v", global.files)
	}
	if _, err := c.Commit("msg", []string{"global/facts.md", "projects/demo/rules.md"}); err == nil {
		t.Fatal("mixed-repo commit succeeded")
	}
}

func TestLayoutV2ReaderWalkAndListRoot(t *testing.T) {
	globalRoot := t.TempDir()
	activeRoot := t.TempDir()
	if err := CreateMissingProjectRepo(globalRoot, true); err != nil {
		t.Fatalf("global scaffold: %v", err)
	}
	if err := CreateMissingProjectRepo(activeRoot, false); err != nil {
		t.Fatalf("active scaffold: %v", err)
	}
	write := func(root, rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(rel))), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(globalRoot, "rules.md", "global")
	write(globalRoot, "agents/coder/persona.md", "persona")
	write(activeRoot, "rules.md", "project")
	write(activeRoot, "episodes/coder/id.md", "episode")

	r := NewLayoutV2Reader(globalRoot, "demo", activeRoot)
	dirs, err := r.ListDirs("")
	if err != nil {
		t.Fatalf("ListDirs root: %v", err)
	}
	if want := []string{"agents", "global", "projects"}; !reflect.DeepEqual(dirs, want) {
		t.Fatalf("root dirs = %#v, want %#v", dirs, want)
	}
	entries, err := r.Walk("")
	if err != nil {
		t.Fatalf("Walk root: %v", err)
	}
	paths := make(map[string]bool)
	for _, e := range entries {
		paths[e.Path] = true
	}
	for _, want := range []string{
		"global/rules.md",
		"agents/coder/persona.md",
		"projects/demo/rules.md",
		"projects/demo/episodes/coder/id.md",
	} {
		if !paths[want] {
			t.Fatalf("Walk root missing %s in %#v", want, paths)
		}
	}
}

func TestLayoutV2ReaderRejectsTraversalAndNonActiveProject(t *testing.T) {
	r := NewLayoutV2Reader(t.TempDir(), "demo", t.TempDir())
	for _, rel := range []string{"../escape", "projects/demo/../..", "projects/other/rules.md"} {
		if _, err := r.Read(rel); err == nil {
			t.Fatalf("Read(%q) succeeded, want error", rel)
		}
	}
}

func TestLayoutV2CommitterRejectsEmptyFiles(t *testing.T) {
	c := &LayoutV2Committer{Reader: NewLayoutV2Reader(t.TempDir(), "demo", t.TempDir())}
	if _, err := c.Commit("msg", nil); err == nil {
		t.Fatal("Commit with no files succeeded")
	}
}
