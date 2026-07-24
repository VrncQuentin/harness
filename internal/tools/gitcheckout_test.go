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
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for missing branch, got none")
	}
}

func TestGitCheckout_MissingRoot(t *testing.T) {
	tool := &gitCheckoutTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}}
	res := tool.Execute(context.Background(), ci, map[string]any{"branch": "main"})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitCheckout_C2MemoryRepoRejected(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitCheckoutTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoPaths: []string{dir}}
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
	ci := CallInfo{SandboxRoots: []string{dir}}
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
}
