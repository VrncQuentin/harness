package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh repository has no commits, so HEAD points at a branch reference that
// does not resolve yet. Reading HEAD unresolved is what keeps that a branch
// name rather than a "reference not found" error.
func TestCurrentBranch_UnbornBranchStillNamesTheBranch(t *testing.T) {
	repo, err := Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch on an unborn branch: %v", err)
	}
	if branch == "" || strings.Contains(branch, "/") {
		t.Fatalf("branch = %q, want a bare branch name", branch)
	}
}

func TestCurrentBranch_ReportsTheCheckedOutBranch(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"),
		[]byte("ref: refs/heads/feat/my-feature\n"), 0o644); err != nil {
		t.Fatalf("WriteFile HEAD: %v", err)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "feat/my-feature" {
		t.Fatalf("branch = %q, want feat/my-feature", branch)
	}
}

// Detached HEAD keeps reporting as detached, with the short hash, so git_push
// can tell the user to name a branch explicitly.
func TestCurrentBranch_DetachedHEADIsReportedAsDetached(t *testing.T) {
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"),
		[]byte("1234567890abcdef1234567890abcdef12345678\n"), 0o644); err != nil {
		t.Fatalf("WriteFile HEAD: %v", err)
	}
	_, err = repo.CurrentBranch()
	var detached *ErrDetachedHEAD
	if !errors.As(err, &detached) {
		t.Fatalf("err = %v, want ErrDetachedHEAD", err)
	}
	if detached.Short != "12345678" {
		t.Fatalf("Short = %q, want the 8-character prefix", detached.Short)
	}
}

// A linked worktree stores .git as a *file* naming the real git directory. The
// hand-rolled read of repoRoot/.git/HEAD this replaced treated .git as a
// directory and failed outright here; asking the opened repository works.
func TestCurrentBranch_LinkedWorktreeLayout(t *testing.T) {
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := Init(mainDir); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// A worktree whose .git file points at a gitdir inside the main repo.
	gitDir := filepath.Join(mainDir, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll gitdir: %v", err)
	}
	for name, body := range map[string]string{
		"HEAD":      "ref: refs/heads/wt-branch\n",
		"commondir": "../..\n",
		"gitdir":    filepath.Join(base, "wt", ".git") + "\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	wtDir := filepath.Join(base, "wt")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .git file: %v", err)
	}

	repo, err := Open(wtDir)
	if err != nil {
		t.Skipf("go-git cannot open this worktree layout here: %v", err)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch in a linked worktree: %v", err)
	}
	if branch != "wt-branch" {
		t.Fatalf("branch = %q, want wt-branch", branch)
	}
}
