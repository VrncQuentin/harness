package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWorkspaceRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return repo
}

func writeRepoFile(t *testing.T, repo *Repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.path, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

func TestStatus(t *testing.T) {
	repo := newWorkspaceRepo(t)

	entries, err := repo.Status()
	if err != nil {
		t.Fatalf("Status on fresh repo: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("fresh repo entries = %+v, want none", entries)
	}

	writeRepoFile(t, repo, "a.txt", "one\n")
	writeRepoFile(t, repo, "b.txt", "two\n")
	if _, err := repo.Commit("initial", []string{"a.txt", "b.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	writeRepoFile(t, repo, "a.txt", "one changed\n") // modified
	writeRepoFile(t, repo, "c.txt", "new\n")         // untracked

	entries, err = repo.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	byPath := map[string]StatusEntry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want a.txt and c.txt", entries)
	}
	if e := byPath["a.txt"]; e.Worktree != 'M' {
		t.Errorf("a.txt worktree code = %q, want M", e.Worktree)
	}
	if e := byPath["c.txt"]; e.Worktree != '?' {
		t.Errorf("c.txt worktree code = %q, want ?", e.Worktree)
	}
}

func TestLog(t *testing.T) {
	repo := newWorkspaceRepo(t)

	entries, err := repo.Log(10)
	if err != nil {
		t.Fatalf("Log on empty repo: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty repo log = %+v, want none", entries)
	}

	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, err := repo.Commit("first commit\n\nbody text", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeRepoFile(t, repo, "a.txt", "two\n")
	sha2, err := repo.Commit("second commit", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	entries, err = repo.Log(1)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Log(1) len = %d, want 1", len(entries))
	}
	if entries[0].SHA != sha2 {
		t.Errorf("newest SHA = %s, want %s", entries[0].SHA, sha2)
	}
	if entries[0].Summary != "second commit" {
		t.Errorf("Summary = %q", entries[0].Summary)
	}

	entries, err = repo.Log(10)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Log(10) len = %d, want 2", len(entries))
	}
	if entries[0].Summary != "second commit" || entries[1].Summary != "first commit" {
		t.Errorf("log order = %q then %q, want newest first", entries[0].Summary, entries[1].Summary)
	}
}

// reflogFile returns the contents of .git/logs/<ref>, or "" when absent. The
// on-disk file is what matters: the git CLI reads these, so an entry written to
// the wrong ref is a wrong answer for the user even if the API round-trips.
func reflogFile(t *testing.T, repo *Repo, ref string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo.path, ".git", "logs", filepath.FromSlash(ref)))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read reflog %s: %v", ref, err)
	}
	return string(data)
}

func TestCheckoutWritesHeadReflog(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	first, err := repo.Commit("first", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, _, err = repo.CreateBranch("feature", ""); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	branchLogBefore := reflogFile(t, repo, "refs/heads/feature")

	preOpBranch, preOpSHA, err := repo.Checkout("feature")
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if preOpSHA != first {
		t.Errorf("preOpSHA = %s, want %s", preOpSHA, first)
	}
	if preOpBranch == "" {
		t.Error("preOpBranch empty, want the branch name before the switch")
	}

	headLog := reflogFile(t, repo, "HEAD")
	if headLog == "" {
		t.Fatal("HEAD reflog is empty; git reflog and HEAD@{n} cannot see the checkout")
	}
	if !strings.Contains(headLog, "checkout: moving from") {
		t.Errorf("HEAD reflog missing the checkout entry:\n%s", headLog)
	}

	// The branch tip did not move, so its own reflog must not claim it did.
	if got := reflogFile(t, repo, "refs/heads/feature"); got != branchLogBefore {
		t.Errorf("target branch reflog changed on checkout:\nbefore: %q\nafter:  %q", branchLogBefore, got)
	}
}

func TestCheckoutRejectsUnknownBranch(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, _, err := repo.Checkout("no-such-branch"); err == nil {
		t.Fatal("Checkout of an unknown branch succeeded, want an error")
	}
	if got := reflogFile(t, repo, "HEAD"); strings.Contains(got, "no-such-branch") {
		t.Errorf("failed checkout still wrote a reflog entry:\n%s", got)
	}
}

func TestDiffCommits(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\ntwo\n")
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeRepoFile(t, repo, "a.txt", "one\nTWO\n")
	if _, err := repo.Commit("second", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	diff, err := repo.DiffCommits(context.Background(), "HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("DiffCommits: %v", err)
	}
	for _, want := range []string{"a.txt", "-two", "+TWO"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}

	if _, err := repo.DiffCommits(context.Background(), "not-a-rev", "HEAD"); err == nil {
		t.Fatal("DiffCommits with bad revision succeeded, want error")
	}
}

func TestDiffWorktree(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\ntwo\nthree\n")
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	diff, err := repo.DiffWorktree(context.Background())
	if err != nil {
		t.Fatalf("DiffWorktree clean: %v", err)
	}
	if diff != "" {
		t.Fatalf("clean worktree diff = %q, want empty", diff)
	}

	writeRepoFile(t, repo, "a.txt", "one\nTWO\nthree\n") // modified
	writeRepoFile(t, repo, "new.txt", "fresh\n")         // untracked addition

	diff, err = repo.DiffWorktree(context.Background())
	if err != nil {
		t.Fatalf("DiffWorktree: %v", err)
	}
	for _, want := range []string{"a.txt", "-two", "+TWO", "new.txt", "+fresh"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
}
