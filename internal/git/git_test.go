package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// initRepo creates a fresh non-bare repo in a temp directory and returns
// both the directory path and an opened *Repo handle. Tests that just
// need a working repo on disk use this instead of duplicating the dance.
func initRepo(t *testing.T) (string, *Repo) {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatalf("plain init: %v", err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return dir, r
}

// writeFile writes data into dir/relPath, creating parent directories as
// needed. relPath uses forward slashes per the Commit contract.
func writeFile(t *testing.T, dir, relPath, data string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
	if err := os.WriteFile(abs, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}

func TestOpen(t *testing.T) {
	t.Run("non-existent path returns error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := Open(missing); err == nil {
			t.Fatal("expected error for missing path, got nil")
		}
	})

	t.Run("non-git directory returns error", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Open(dir); err == nil {
			t.Fatal("expected error for non-git directory, got nil")
		}
	})

	t.Run("freshly initialised repo opens", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := gogit.PlainInit(dir, false); err != nil {
			t.Fatalf("plain init: %v", err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if r == nil {
			t.Fatal("expected non-nil Repo handle")
		}
	})
}

func TestCommit(t *testing.T) {
	dir, r := initRepo(t)

	writeFile(t, dir, "projects/global/episodes/coder/2026-04-26.md", "first episode")
	msg := BuildMessage(map[string]string{"agent": "coder", "type": "episode"}, "first session summary")

	sha, err := r.Commit(msg, []string{"projects/global/episodes/coder/2026-04-26.md"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(sha) != 40 {
		t.Errorf("expected 40-char hex SHA, got %q", sha)
	}

	head, err := r.repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Hash().String() != sha {
		t.Fatalf("HEAD = %s, want committed sha %s", head.Hash(), sha)
	}
	commit, err := r.repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	if commit.Message != msg {
		t.Fatalf("commit message = %q, want %q", commit.Message, msg)
	}
	if commit.Author.Name == "" || commit.Author.Email == "" {
		t.Fatalf("commit author not populated: %+v", commit.Author)
	}
}

func TestInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Init(dir)
	if err != nil {
		t.Fatalf("Init new repo: %v", err)
	}
	if r == nil {
		t.Fatal("Init returned nil repo")
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("Open after Init: %v", err)
	}

	again, err := Init(dir)
	if err != nil {
		t.Fatalf("Init existing repo: %v", err)
	}
	if again == nil {
		t.Fatal("Init existing returned nil repo")
	}
}

// TestErrors exercises the error paths that other tests rely on. Each
// returned error wraps the underlying problem with a "git:" prefix per
// the package convention; tests assert the prefix as a regression guard.
func TestErrors(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.HasPrefix(err.Error(), "git: ") {
		t.Errorf("error not wrapped: %v", err)
	}
}

func TestCurrentBranch_LinkedWorktreeLayout(t *testing.T) {
	// Simulate a linked worktree: .git is a file containing the path
	// to the common directory containing HEAD, not a directory itself.
	dir := t.TempDir()
	commonDir := filepath.Join(dir, "common")
	if err := os.MkdirAll(commonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "HEAD"), []byte("ref: refs/heads/feat/linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(worktree, ".git")
	content := "gitdir: " + filepath.ToSlash(commonDir) + "\n"
	if err := os.WriteFile(gitFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(worktree)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		t.Fatal(err)
	}
	if branch != "feat/linked" {
		t.Errorf("expected feat/linked, got %s", branch)
	}
}

func TestCurrentBranch_DetachedHead(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("abcdef1234567890abcdef1234567890abcdef12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CurrentBranch()
	if err == nil || !strings.Contains(err.Error(), "detached") {
		t.Errorf("expected detached HEAD error, got %v", err)
	}
}

func TestCurrentBranch_RejectsNonBranchSymbolic(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/tags/v1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CurrentBranch()
	if err == nil || !strings.Contains(err.Error(), "non-branch") {
		t.Errorf("expected non-branch ref error, got %v", err)
	}
}
