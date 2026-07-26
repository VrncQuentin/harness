package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// initRepoWithCommit initialises a git repo at dir with one initial commit
// so that the HEAD reference exists.
func initRepoWithCommit(t *testing.T, dir string) {
	t.Helper()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Add README: %v", err)
	}
	if _, err := wt.Commit("init", &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit init: %v", err)
	}
}

func TestGitCommit_MissingMessage(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitCommitTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for missing message, got none")
	}
}

func TestGitCommit_MissingRoot(t *testing.T) {
	tool := &gitCommitTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}}
	res := tool.Execute(context.Background(), ci, map[string]any{"message": "hi"})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitCommit_SandboxViolation(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)
	other := t.TempDir()

	tool := &gitCommitTool{}
	ci := CallInfo{SandboxRoots: []string{other}} // dir not in sandbox
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "message": "hi"})
	if res.Error == "" {
		t.Fatal("expected sandbox violation error, got none")
	}
}

func TestGitCommit_C2MemoryRepoRejected(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitCommitTool{}
	// dir is in sandbox but also listed as a memory repo — must be rejected.
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoPaths: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "message": "evil"})
	if res.Error == "" {
		t.Fatal("expected C2 scope error, got none")
	}
	if !strings.Contains(res.Error, "C2") {
		t.Fatalf("expected C2 in error, got %q", res.Error)
	}
}

func TestGitCommit_StageAllAndCommit(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	// Create a new file — not yet staged.
	f := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tool := &gitCommitTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	// files omitted → stage all
	res := tool.Execute(context.Background(), ci, map[string]any{
		"root":    dir,
		"message": "add hello",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "committed") {
		t.Fatalf("expected 'committed' in result, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "pre-op SHA") {
		t.Fatalf("expected pre-op SHA in result, got %q", res.Content)
	}
}

func TestGitCommit_StageSpecificFiles(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(f1, []byte("a\n"), 0o644); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := os.WriteFile(f2, []byte("b\n"), 0o644); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	tool := &gitCommitTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{
		"root":    dir,
		"message": "add a only",
		"files":   []any{"a.txt"},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "committed") {
		t.Fatalf("expected 'committed' in result, got %q", res.Content)
	}
}

func TestIsMemoryRepo(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()

	tests := []struct {
		name        string
		absRoot     string
		memoryPaths []string
		want        bool
	}{
		{name: "matches its own memory path", absRoot: dir, memoryPaths: []string{dir}, want: true},
		{name: "different directories", absRoot: dir, memoryPaths: []string{other}},
		{name: "no memory paths", absRoot: dir},
		{name: "empty memory path entry", absRoot: dir, memoryPaths: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isMemoryRepo(tt.absRoot, tt.memoryPaths)
			if err != nil {
				t.Fatalf("isMemoryRepo: %v", err)
			}
			if got != tt.want {
				t.Errorf("isMemoryRepo = %v, want %v", got, tt.want)
			}
		})
	}
}

// The predicate guards a hard lock, so a path it cannot locate has to surface
// as an error. Reporting false would tell the caller the root is outside every
// memory repo, which is the one thing that is not known.
func TestIsMemoryRepoReportsUnresolvablePaths(t *testing.T) {
	dir := t.TempDir()
	unresolvable := filepath.Join(dir, "bad\x00name")

	if _, err := isMemoryRepo(unresolvable, []string{dir}); err == nil {
		t.Error("unresolvable root returned no error; the C2 lock would see a plain false")
	}
	if _, err := isMemoryRepo(dir, []string{unresolvable}); err == nil {
		t.Error("unresolvable memory path returned no error; the C2 lock would see a plain false")
	}
}
