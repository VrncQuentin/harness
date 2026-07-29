package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitPushTool_MissingRoot(t *testing.T) {
	tool := &gitPushTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{})
	if res.Error == "" {
		t.Fatal("expected error for missing root")
	}
}

func TestGitPushTool_DetachedHEAD(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("abc123def456\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &gitPushTool{}
	c := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), c, map[string]any{"root": dir})
	if res.Error == "" || !strings.Contains(res.Error, "detached") {
		t.Errorf("expected detached HEAD error, got error=%q content=%q", res.Error, res.Content)
	}
}

func TestGitPushTool_Proposal(t *testing.T) {
	dir := initRepoWithBranch(t, "feat/my-feature")
	tool := &gitPushTool{}
	c := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), c, map[string]any{"root": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !res.Proposal {
		t.Error("result.Proposal should be true for git_push")
	}
	if !strings.Contains(res.Content, "PROPOSAL") {
		t.Error("content should contain PROPOSAL marker")
	}
	if !strings.Contains(res.Content, "feat/my-feature") {
		t.Errorf("content should mention branch, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "origin") {
		t.Errorf("content should mention default remote 'origin', got: %s", res.Content)
	}
}

func TestGitPushTool_ExplicitBranchAndRemote(t *testing.T) {
	dir := initRepoWithBranch(t, "main")
	tool := &gitPushTool{}
	c := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), c, map[string]any{
		"root":   dir,
		"remote": "upstream",
		"branch": "release/v2",
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "upstream") {
		t.Error("content should mention remote 'upstream'")
	}
	if !strings.Contains(res.Content, "release/v2") {
		t.Error("content should mention explicit branch")
	}
}

func TestGitPushTool_ForceFlag(t *testing.T) {
	dir := initRepoWithBranch(t, "main")
	tool := &gitPushTool{}
	c := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: noMemoryRepos()}
	res := tool.Execute(context.Background(), c, map[string]any{"root": dir, "force": true})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "force-with-lease") {
		t.Errorf("force push should use --force-with-lease, got: %s", res.Content)
	}
}

func TestGitPushTool_C2MemoryRepoRejected(t *testing.T) {
	dir := initRepoWithBranch(t, "main")
	tool := &gitPushTool{}
	c := CallInfo{SandboxRoots: []string{dir}, MemoryRepoCheck: memoryScopeOver(dir)}
	res := tool.Execute(context.Background(), c, map[string]any{"root": dir})
	if res.Error == "" || !strings.Contains(res.Error, "C2") {
		t.Errorf("expected C2 scope error, got error=%q", res.Error)
	}
}

func initRepoWithBranch(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
