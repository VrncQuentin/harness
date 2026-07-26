package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

const outOfSandboxSecret = "SUPER-SECRET-OUT-OF-SANDBOX-CONTENT"

// mustSymlink creates a symlink or skips the test. Unprivileged Windows
// sessions cannot create them, so this path is exercised on Linux CI.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
}

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. A junction needs no special
// privilege and is traversed by both os.ReadFile and filepath.EvalSymlinks
// exactly like a symlink, so it exercises the same escape path on developer
// machines where symlink creation is denied.
func mustLinkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

func TestWorktreeContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name        string
		relPath     string
		wantContent string
		wantBinary  bool
	}{
		{name: "regular file", relPath: "real.txt", wantContent: "hello\n"},
		{name: "binary file", relPath: "bin.dat", wantBinary: true},
		{name: "missing file", relPath: "gone.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, isBinary := worktreeContent(root, tt.relPath)
			if content != tt.wantContent {
				t.Errorf("content = %q, want %q", content, tt.wantContent)
			}
			if isBinary != tt.wantBinary {
				t.Errorf("isBinary = %v, want %v", isBinary, tt.wantBinary)
			}
		})
	}
}

func TestWorktreeContent_RejectsPathBelowLinkedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outOfSandboxSecret), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	mustLinkDir(t, outside, filepath.Join(root, "leakdir"))

	// Positive control: the escape defense must not reject in-root reads.
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("in root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if content, _ := worktreeContent(root, "real.txt"); content != "in root\n" {
		t.Fatalf("in-root read = %q, want %q", content, "in root\n")
	}

	// Lexically inside the repo, physically outside it.
	if content, _ := worktreeContent(root, "leakdir/secret.txt"); content != "" {
		t.Errorf("read through linked parent returned %q, want empty", content)
	}
	// The link itself never yields the target file's bytes. What it does yield
	// depends on the link kind, and both are correct: a symlink diffs as its
	// target path, which is how git stores one, while a Windows junction is
	// reported irregular and contributes nothing.
	if content, _ := worktreeContent(root, "leakdir"); strings.Contains(content, outOfSandboxSecret) {
		t.Errorf("directory link content = %q, leaked content from outside the repo", content)
	}
}

func TestDiffWorktree_DoesNotFollowLinksOutOfRepo(t *testing.T) {
	outside := t.TempDir()
	secretFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretFile, []byte(outOfSandboxSecret+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}

	tests := []struct {
		name   string
		link   string // repo-relative link path
		target string
		mk     func(t *testing.T, target, link string)
	}{
		{name: "link to file outside repo", link: "leak.txt", target: secretFile, mk: mustSymlink},
		{name: "link to directory outside repo", link: "leakdir", target: outside, mk: mustLinkDir},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newWorkspaceRepo(t)
			writeRepoFile(t, repo, "a.txt", "one\n")
			if _, err := repo.Commit("initial", []string{"a.txt"}); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			tt.mk(t, tt.target, filepath.Join(repo.path, tt.link))

			diff, err := repo.DiffWorktree(context.Background())
			if err != nil {
				t.Fatalf("DiffWorktree: %v", err)
			}
			if strings.Contains(diff, outOfSandboxSecret) {
				t.Errorf("diff leaked content from outside the repo:\n%s", diff)
			}
		})
	}
}

func TestDiffWorktree_SymlinkDiffedAsTarget(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, err := repo.Commit("initial", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	mustSymlink(t, "a.txt", filepath.Join(repo.path, "link.txt"))

	diff, err := repo.DiffWorktree(context.Background())
	if err != nil {
		t.Fatalf("DiffWorktree: %v", err)
	}
	// git records a symlink's blob as its target path, not the target's bytes.
	if !strings.Contains(diff, "+a.txt") {
		t.Errorf("diff missing symlink target as added content:\n%s", diff)
	}
	if strings.Contains(diff, "+one") {
		t.Errorf("diff dereferenced the symlink into its target's content:\n%s", diff)
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
