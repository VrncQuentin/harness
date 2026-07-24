package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
)

func TestGitStatusTool_CleanRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}

	tool := &gitStatusTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "clean") {
		t.Fatalf("expected clean status, got %q", res.Content)
	}
}

func TestGitStatusTool_DirtyRepo(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	f := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(f, []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := wt.Add("hello.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	tool := &gitStatusTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})

	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "hello.txt") {
		t.Fatalf("expected hello.txt in status, got %q", res.Content)
	}
}

func TestGitStatusTool_MissingRoot(t *testing.T) {
	tool := &gitStatusTool{}
	ci := CallInfo{SandboxRoots: []string{t.TempDir()}}
	res := tool.Execute(context.Background(), ci, map[string]any{})
	if res.Error == "" {
		t.Fatal("expected error for missing root, got none")
	}
}

func TestGitStatusTool_NotARepo(t *testing.T) {
	dir := t.TempDir()
	tool := &gitStatusTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for non-git dir, got none")
	}
	if !strings.Contains(res.Error, "not a git repository") {
		t.Fatalf("expected 'not a git repository' error, got %q", res.Error)
	}
}
