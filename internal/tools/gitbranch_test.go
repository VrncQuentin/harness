package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGitBranch_MissingName(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for missing name, got none")
	}
}

func TestGitBranch_MissingRoot(t *testing.T) {
	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}}
	res := tool.Execute(context.Background(), ci, map[string]any{"name": "new-branch"})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitBranch_C2MemoryRepoRejected(t *testing.T) {
	dir := t.TempDir()
	initRepoWithCommit(t, dir)

	tool := &gitBranchTool{}
	ci := CallInfo{SandboxRoots: []string{dir}, MemoryRepoPaths: []string{dir}}
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
	ci := CallInfo{SandboxRoots: []string{dir}}
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
}
