package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGitCheckout_MissingBranch(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitCheckoutTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for missing branch, got none")
	}
}

func TestGitCheckout_MissingRoot(t *testing.T) {
	tool := &gitCheckoutTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), ci, map[string]any{"branch": "main"})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitCheckout_C2MemoryRepoRejected(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitCheckoutTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: memoryScopeOver(dir)}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "branch": "main"})
	if res.Error == "" {
		t.Fatal("expected C2 scope error, got none")
	}
	if !strings.Contains(res.Error, "C2") {
		t.Fatalf("expected C2 in error, got %q", res.Error)
	}
}

func TestGitCheckout_SwitchBranch(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	// Create a second branch to switch to using git_branch.
	branchTool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := branchTool.Execute(context.Background(), ci, map[string]any{
		"root": dir,
		"name": "feature",
	})
	if res.Error != "" {
		t.Fatalf("git_branch: %s", res.Error)
	}

	tool := &gitCheckoutTool{}
	res = tool.Execute(context.Background(), ci, map[string]any{
		"root":   dir,
		"branch": "feature",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "feature") {
		t.Fatalf("expected branch name in result, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "pre-op branch") {
		t.Fatalf("expected pre-op branch in result, got %q", res.Content)
	}
	// The undo is described, not rendered as a command: git accepts branch
	// names containing shell metacharacters.
	if strings.Contains(res.Content, "git checkout ") {
		t.Errorf("result renders a runnable checkout command:\n%s", res.Content)
	}
}

// A reflog that could not be written must ride along as a WARNING rather than
// turning a completed checkout into a failed tool call.
func TestGitCheckout_ReflogFailureWarnsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	branchRes := (&gitBranchTool{}).Execute(context.Background(), ci, map[string]any{"root": dir, "name": "feature"})
	if branchRes.Error != "" {
		t.Fatalf("git_branch: %s", branchRes.Error)
	}
	breakToolReflogs(t, dir)

	res := (&gitCheckoutTool{}).Execute(context.Background(), ci, map[string]any{"root": dir, "branch": "feature"})
	if res.Error != "" {
		t.Fatalf("checkout reported an error for a reflog problem: %s", res.Error)
	}
	if !strings.Contains(res.Content, "switched to branch") {
		t.Errorf("result does not report the checkout that happened:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "WARNING:") {
		t.Errorf("result carries no WARNING for the unwritten reflog:\n%s", res.Content)
	}
}
