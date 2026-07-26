package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitBranch_MissingName(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for missing name, got none")
	}
}

func TestGitBranch_MissingRoot(t *testing.T) {
	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), ci, map[string]any{"name": "new-branch"})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitBranch_C2MemoryRepoRejected(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: memoryScopeOver(dir)}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "name": "bad"})
	if res.Error == "" {
		t.Fatal("expected C2 scope error, got none")
	}
	if !strings.Contains(res.Error, "C2") {
		t.Fatalf("expected C2 in error, got %q", res.Error)
	}
}

func TestGitBranch_CreatesFromHEAD(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), ci, map[string]any{
		"root": dir,
		"name": "feature-x",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "feature-x") {
		t.Fatalf("expected branch name in result, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "pre-op HEAD SHA") {
		t.Fatalf("expected pre-op SHA in result, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "undo: delete local branch") {
		t.Fatalf("expected the undo instruction in result, got %q", res.Content)
	}
}

// breakToolReflogs replaces .git/logs with a regular file so every reflog write
// in the repository fails, without disturbing the operation itself.
func breakToolReflogs(t *testing.T, dir string) {
	t.Helper()
	logs := filepath.Join(dir, ".git", "logs")
	if err := os.RemoveAll(logs); err != nil {
		t.Fatalf("RemoveAll logs: %v", err)
	}
	if err := os.WriteFile(logs, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("WriteFile logs: %v", err)
	}
}

// A reflog that could not be written must ride along as a WARNING rather than
// turning a created branch into a failed tool call.
func TestGitBranch_ReflogFailureWarnsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)
	breakToolReflogs(t, dir)

	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := (&gitBranchTool{}).Execute(context.Background(), ci, map[string]any{"root": dir, "name": "feature"})

	if res.Error != "" {
		t.Fatalf("branch creation reported an error for a reflog problem: %s", res.Error)
	}
	if !strings.Contains(res.Content, "created branch") {
		t.Errorf("result does not report the branch that was created:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "WARNING:") {
		t.Errorf("result carries no WARNING for the unwritten reflog:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "undo: delete local branch") {
		t.Errorf("result dropped the undo instruction:\n%s", res.Content)
	}
}

// git's ref-format rules permit shell metacharacters in a branch name, so the
// undo hint must describe the action rather than render a runnable command.
// Emitting "git branch -D <name>" would hand the reader — or anything that
// scrapes tool output for commands — a line that executes whatever the name
// encodes.
func TestGitBranch_UndoHintIsNotExecutable(t *testing.T) {
	// Each of these is accepted by git and creatable on both platforms.
	names := []string{"safe;whoami", "safe$(whoami)", "safe`id`", "quote'name"}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			initRepoWithCommit(t, dir)

			tool := &gitBranchTool{}
			ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
			res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "name": name})
			if res.Error != "" {
				t.Fatalf("unexpected error for a valid branch name: %s", res.Error)
			}

			// No runnable git command may appear anywhere in the output.
			if strings.Contains(res.Content, "git branch -D") {
				t.Errorf("output renders a runnable delete command:\n%s", res.Content)
			}
			// The name must be quoted, so the metacharacters are inert text.
			if !strings.Contains(res.Content, fmt.Sprintf("%q", name)) {
				t.Errorf("branch name is not quoted in the output:\n%s", res.Content)
			}
		})
	}
}
