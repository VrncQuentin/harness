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

func gitCommit(t *testing.T, repo *gogit.Repository, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
	if _, err := wt.Commit("add "+name, &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "t@t.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestGitDiffTool_WorktreeNoChanges(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	gitCommit(t, repo, dir, "a.txt", "hello\n")

	tool := &gitDiffTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "no changes") {
		t.Fatalf("expected 'no changes', got %q", res.Content)
	}
}

func TestGitDiffTool_WorktreeDirty(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	gitCommit(t, repo, dir, "a.txt", "hello\n")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("WriteFile dirty: %v", err)
	}

	tool := &gitDiffTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "world") {
		t.Fatalf("expected 'world' in diff, got %q", res.Content)
	}
}

func TestGitDiffTool_RevisionDiff(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	gitCommit(t, repo, dir, "a.txt", "first\n")
	gitCommit(t, repo, dir, "b.txt", "second\n")

	tool := &gitDiffTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "from": "HEAD~1"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "b.txt") {
		t.Fatalf("expected b.txt in diff, got %q", res.Content)
	}
}

func TestGitDiffTool_ToWithoutFrom(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	tool := &gitDiffTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "to": "HEAD"})
	if res.Error == "" {
		t.Fatal("expected error for to without from")
	}
	if !strings.Contains(res.Error, "to requires from") {
		t.Fatalf("expected 'to requires from', got %q", res.Error)
	}
}

func TestGitDiffTool_NotARepo(t *testing.T) {
	dir := t.TempDir()
	tool := &gitDiffTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for non-git dir")
	}
}
