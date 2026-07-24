package tools

import (
	"context"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
)

func TestGitLogTool_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	tool := &gitLogTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "no commits") {
		t.Fatalf("expected 'no commits', got %q", res.Content)
	}
}

func TestGitLogTool_WithCommits(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	gitCommit(t, repo, dir, "a.txt", "first\n")
	gitCommit(t, repo, dir, "b.txt", "second\n")

	tool := &gitLogTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "2 commit(s)") {
		t.Fatalf("expected '2 commit(s)', got %q", res.Content)
	}
	if !strings.Contains(res.Content, "add b.txt") {
		t.Fatalf("expected most recent commit first, got %q", res.Content)
	}
}

func TestGitLogTool_NClamp(t *testing.T) {
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	for i := range 3 {
		gitCommit(t, repo, dir, "f"+string(rune('a'+i))+".txt", "x\n")
	}

	tool := &gitLogTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}

	// n=1 should return only 1 commit
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir, "n": float64(1)})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !strings.Contains(res.Content, "1 commit(s)") {
		t.Fatalf("expected '1 commit(s)', got %q", res.Content)
	}

	// n=200 should be clamped to gitLogMaxN
	res = tool.Execute(context.Background(), ci, map[string]any{"root": dir, "n": float64(200)})
	if res.Error != "" {
		t.Fatalf("unexpected error (n=200): %s", res.Error)
	}
	if strings.Contains(res.Content, "200 commit(s)") {
		t.Fatalf("n=200 was not clamped, got %q", res.Content)
	}
}

func TestGitLogTool_NotARepo(t *testing.T) {
	dir := t.TempDir()
	tool := &gitLogTool{}
	ci := CallInfo{SandboxRoots: []string{dir}}
	res := tool.Execute(context.Background(), ci, map[string]any{"root": dir})
	if res.Error == "" {
		t.Fatal("expected error for non-git dir")
	}
}
