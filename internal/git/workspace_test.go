package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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

<<<<<<< HEAD
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
=======
// stagedPaths returns the paths currently in the index.
func stagedPaths(t *testing.T, repo *Repo) []string {
	t.Helper()
	idx, err := repo.repo.Storer.Index()
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	paths := make([]string, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		paths = append(paths, e.Name)
	}
	sort.Strings(paths)
	return paths
}

func TestWorkspaceStageAndCommit(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	writeRepoFile(t, repo, "b.txt", "two\n")

	newSHA, preOpSHA, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first")
	if err != nil {
		t.Fatalf("WorkspaceStageAndCommit: %v", err)
	}
	if newSHA == "" {
		t.Error("newSHA empty")
	}
	if preOpSHA != "" {
		t.Errorf("preOpSHA = %q on an initial commit, want empty", preOpSHA)
	}
	if got := stagedPaths(t, repo); len(got) != 1 || got[0] != "a.txt" {
		t.Errorf("staged = %v, want only a.txt", got)
	}

	// Second commit: empty file list stages everything.
	writeRepoFile(t, repo, "a.txt", "one changed\n")
	second, preOp2, err := repo.WorkspaceStageAndCommit(nil, "second")
	if err != nil {
		t.Fatalf("WorkspaceStageAndCommit all: %v", err)
	}
	if preOp2 != newSHA {
		t.Errorf("preOpSHA = %s, want the previous HEAD %s", preOp2, newSHA)
	}
	if second == newSHA {
		t.Error("second commit produced the same SHA as the first")
	}
	if got := stagedPaths(t, repo); len(got) != 2 {
		t.Errorf("staged = %v, want a.txt and b.txt", got)
	}
}

// A failed write must not leave behind the part of the staging it completed.
// The file list is staged entry by entry, so a bad path partway through used to
// leave every earlier file staged while the call reported failure — the user's
// index silently gained content from a commit that never happened.
func TestWorkspaceStageAndCommitRollsBackPartialStaging(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "committed.txt", "one\n")
	if _, _, err := repo.WorkspaceStageAndCommit([]string{"committed.txt"}, "first"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	before := stagedPaths(t, repo)

	// The first path stages cleanly; the second does not exist, so the call
	// fails with "good.txt" already in the index.
	writeRepoFile(t, repo, "good.txt", "staged before the failure\n")
	_, _, err := repo.WorkspaceStageAndCommit([]string{"good.txt", "no-such-file.txt"}, "doomed")
	if err == nil {
		t.Fatal("staging a nonexistent file succeeded, want an error")
	}

	after := stagedPaths(t, repo)
	for _, p := range after {
		if p == "good.txt" {
			t.Errorf("index = %v, still holds good.txt from the failed call", after)
		}
	}
	if len(after) != len(before) {
		t.Errorf("index after failed call = %v, want it restored to %v", after, before)
	}
}

// The rollback mechanism itself: a snapshot must be able to undo arbitrary
// staging, and must not alias the live index it was copied from.
func TestSnapshotAndRestoreIndex(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, _, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	snapshot, err := repo.snapshotIndex()
	if err != nil {
		t.Fatalf("snapshotIndex: %v", err)
	}
	snapshotLen := len(snapshot.Entries)

	wt, err := repo.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	writeRepoFile(t, repo, "b.txt", "two\n")
	if err = repo.stageLocked(wt, []string{"b.txt"}); err != nil {
		t.Fatalf("stageLocked: %v", err)
	}
	if got := stagedPaths(t, repo); len(got) != snapshotLen+1 {
		t.Fatalf("staged = %v, want one more than the snapshot's %d", got, snapshotLen)
	}
	// Staging must not have mutated the snapshot through a shared backing array.
	if len(snapshot.Entries) != snapshotLen {
		t.Errorf("snapshot grew to %d entries, want %d — it aliases the live index", len(snapshot.Entries), snapshotLen)
	}

	if err = repo.restoreIndex(snapshot); err != nil {
		t.Fatalf("restoreIndex: %v", err)
	}
	got := stagedPaths(t, repo)
	if len(got) != snapshotLen || (len(got) > 0 && got[0] != "a.txt") {
		t.Errorf("index after restore = %v, want just a.txt", got)
	}
}

// Handles are opened per tool call, so two concurrent commits hold two
// different Repo structs. Serialization has to key on the repository, not the
// handle, or one task's commit sweeps up the other's staged files.
func TestWorkspaceStageAndCommitSerializesAcrossHandles(t *testing.T) {
	dir := t.TempDir()
	seed, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeRepoFile(t, seed, "base.txt", "base\n")
	if _, _, err := seed.WorkspaceStageAndCommit([]string{"base.txt"}, "base"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	const workers = 4
	for i := range workers {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// A separate handle per goroutine, exactly as each tool call gets.
			handle, oerr := Open(dir)
			if oerr != nil {
				errs <- oerr
				return
			}
			<-start
			file := fmt.Sprintf("f%d.txt", n)
			if _, _, cerr := handle.WorkspaceStageAndCommit([]string{file}, "commit "+file); cerr != nil {
				errs <- cerr
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent commit: %v", err)
	}

	// Each commit must be its own; nothing may be lost or double-counted.
	entries, err := seed.Log(workers + 5)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(entries) != workers+1 {
		t.Errorf("log has %d commits, want %d (base + %d workers)", len(entries), workers+1, workers)
>>>>>>> b8f8c33 (fix(git): commit staging and creation as one reversible operation)
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
