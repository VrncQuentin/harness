package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

func newWorkspaceRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	repo, err := Init(dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
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

// pinWorktree opens dir the way DiffWorktree does, so the helper tests below
// exercise the same rooted access the diff path uses.
func pinWorktree(t *testing.T, dir string) *rootfs.Root {
	t.Helper()
	root, err := rootfs.Open(dir)
	if err != nil {
		t.Fatalf("rootfs.Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestWorktreeContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	root := pinWorktree(t, dir)

	tests := []struct {
		name        string
		relPath     string
		wantContent string
		wantBinary  bool
	}{
		{name: "regular file", relPath: "real.txt", wantContent: "hello\n"},
		{name: "nested file", relPath: "sub/nested.txt", wantContent: "nested\n"},
		{name: "binary file", relPath: "bin.dat", wantBinary: true},
		{name: "missing file", relPath: "gone.txt"},
		{name: "the repository itself", relPath: "."},
		{name: "a path that climbs out", relPath: "../outside.txt"},
		{name: "an absolute path", relPath: filepath.Join(t.TempDir(), "elsewhere.txt")},
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
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outOfSandboxSecret), 0o600); err != nil {
		t.Fatalf("WriteFile secret: %v", err)
	}
	mustLinkDir(t, outside, filepath.Join(dir, "leakdir"))

	// Positive control: the escape defense must not reject in-root reads.
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("in root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	root := pinWorktree(t, dir)
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

// A directory link that stays inside the repository is followed, because git
// follows it too. The hand-written component walk this replaced refused every
// reparse point, so in-repo content behind one was silently absent from the
// diff. Containment, not the presence of a link, is the property under test.
func TestWorktreeContent_FollowsInRepoLink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "nested.txt"), []byte("in repo\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// The link has to be relative: os.Root refuses an absolute link target even
	// when it points back inside the root, and a Windows junction always stores
	// an absolute target. So this is a symlink or nothing.
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	root := pinWorktree(t, dir)
	if content, _ := worktreeContent(root, "alias/nested.txt"); content != "in repo\n" {
		t.Errorf("read through an in-repo link = %q, want %q", content, "in repo\n")
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
	if _, _, _, err = repo.CreateBranch("feature", ""); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	branchLogBefore := reflogFile(t, repo, "refs/heads/feature")

	preOpBranch, preOpSHA, _, err := repo.Checkout("feature")
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

	if _, _, _, err := repo.Checkout("no-such-branch"); err == nil {
		t.Fatal("Checkout of an unknown branch succeeded, want an error")
	}
	if got := reflogFile(t, repo, "HEAD"); strings.Contains(got, "no-such-branch") {
		t.Errorf("failed checkout still wrote a reflog entry:\n%s", got)
	}
}

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

	newSHA, preOpSHA, _, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first")
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
	second, preOp2, _, err := repo.WorkspaceStageAndCommit(nil, "second")
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
	if _, _, _, err := repo.WorkspaceStageAndCommit([]string{"committed.txt"}, "first"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	before := stagedPaths(t, repo)

	// The first path stages cleanly; the second does not exist, so the call
	// fails with "good.txt" already in the index.
	writeRepoFile(t, repo, "good.txt", "staged before the failure\n")
	_, _, _, err := repo.WorkspaceStageAndCommit([]string{"good.txt", "no-such-file.txt"}, "doomed")
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
	if _, _, _, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first"); err != nil {
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
	defer seed.Close()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeRepoFile(t, seed, "base.txt", "base\n")
	if _, _, _, err := seed.WorkspaceStageAndCommit([]string{"base.txt"}, "base"); err != nil {
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
			defer handle.Close()
			if oerr != nil {
				errs <- oerr
				return
			}
			<-start
			file := fmt.Sprintf("f%d.txt", n)
			if _, _, _, cerr := handle.WorkspaceStageAndCommit([]string{file}, "commit "+file); cerr != nil {
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
	}
}

// shortPathName returns the 8.3 alias of dir, or "" when the volume has none.
// Windows CI runners have 8.3 generation on — their temp directory is the
// RUNNER~1 form — while many developer volumes have it disabled.
func shortPathName(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return ""
	}
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		"(New-Object -ComObject Scripting.FileSystemObject).GetFolder('"+dir+"').ShortPath").CombinedOutput()
	if err != nil {
		return ""
	}
	short := strings.TrimSpace(string(out))
	if short == dir {
		return ""
	}
	return short
}

// Every spelling of one repository must select one mutation lock. The lock was
// keyed through filepath.EvalSymlinks, which canonicalizes case and resolves
// "." and ".." but leaves an 8.3 short name alone — so a repository reached by
// its short path and by its long path took two different mutexes and the two
// handles serialized against nothing.
func TestMutationLockIdentityAcrossSpellings(t *testing.T) {
	base := t.TempDir()
	repoPath := filepath.Join(base, "ProjectRepositoryDirectoryLongName")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	seed, err := Init(repoPath)
	defer seed.Close()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeRepoFile(t, seed, "base.txt", "base\n")
	if _, _, _, err = seed.WorkspaceStageAndCommit([]string{"base.txt"}, "base"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	spellings := map[string]string{
		"as given":      repoPath,
		"trailing sep":  repoPath + string(filepath.Separator),
		"dot component": filepath.Join(repoPath, "."),
		"parent bounce": filepath.Join(repoPath, "..", filepath.Base(repoPath)),
	}
	if runtime.GOOS == "windows" {
		spellings["upper case"] = strings.ToUpper(repoPath)
	}
	// The case that specifically catches the old EvalSymlinks key: it does not
	// resolve 8.3 aliases, so long and short forms hashed differently.
	if short := shortPathName(t, repoPath); short != "" {
		spellings["8.3 short name"] = short
	} else {
		t.Log("no 8.3 alias on this volume; the short-name case runs on Windows CI")
	}

	for name, spelling := range spellings {
		t.Run(name, func(t *testing.T) {
			handle, oerr := Open(spelling)
			defer handle.Close()
			if oerr != nil {
				t.Fatalf("Open(%s): %v", spelling, oerr)
			}
			if !handle.Identity().Equal(seed.Identity()) {
				t.Errorf("identity differs, so these handles do not share a coordinator:\n  seed:  %s\n  %s: %s",
					seed.Identity(), name, handle.Identity())
			}
			if handle.gate != seed.gate {
				t.Errorf("handles acquired different coordinator instances for one repository")
			}
		})
	}

	// The shared key must also hand back the same mutex under real contention.
	const workers = 6
	for i := range workers {
		if err := os.WriteFile(filepath.Join(repoPath, fmt.Sprintf("f%d.txt", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	alternates := make([]string, 0, len(spellings))
	for _, spelling := range spellings {
		alternates = append(alternates, spelling)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Writers arrive through different spellings of the same repository.
			handle, oerr := Open(alternates[n%len(alternates)])
			defer handle.Close()
			if oerr != nil {
				errs <- oerr
				return
			}
			<-start
			file := fmt.Sprintf("f%d.txt", n)
			if _, _, _, cerr := handle.WorkspaceStageAndCommit([]string{file}, "commit "+file); cerr != nil {
				errs <- cerr
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent commit through mixed spellings: %v", err)
	}

	entries, err := seed.Log(workers + 5)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(entries) != workers+1 {
		t.Errorf("log has %d commits, want %d (base + %d writers)", len(entries), workers+1, workers)
	}
}

// go-git refuses to open a repository reached through a directory link, so a
// second handle cannot be created that way and the lock never sees the alias.
// Pinning this keeps the identity test honest about which spellings are
// actually reachable, and turns a future relaxation of that restriction into a
// visible failure rather than a silent hole.
func TestOpenThroughDirectoryLinkIsRefused(t *testing.T) {
	base := t.TempDir()
	repoPath := filepath.Join(base, "repo")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	repo, err := Init(repoPath)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = repo.Close() }()
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, repoPath, alias)

	if _, err := Open(alias); err == nil {
		t.Error("Open through a directory link succeeded; the mutation lock must then key it onto the same repository")
	}
}

// A checkout moves HEAD, the index, and the worktree. Serializing commits
// against each other but not against a checkout leaves it free to run between a
// commit's staging and its wt.Commit, so the commit builds on state that has
// already been replaced.
func TestCheckoutSerializesAgainstStageAndCommit(t *testing.T) {
	dir := t.TempDir()
	seed, err := Init(dir)
	defer seed.Close()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeRepoFile(t, seed, "base.txt", "base\n")
	if _, _, _, err = seed.WorkspaceStageAndCommit([]string{"base.txt"}, "base"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, _, _, err = seed.CreateBranch("other", ""); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	const rounds = 8
	for i := range rounds {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, rounds*2)
	start := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		handle, oerr := Open(dir)
		defer handle.Close()
		if oerr != nil {
			errs <- oerr
			return
		}
		<-start
		for i := range rounds {
			if _, _, _, cerr := handle.WorkspaceStageAndCommit(
				[]string{fmt.Sprintf("f%d.txt", i)}, fmt.Sprintf("commit %d", i)); cerr != nil {
				errs <- cerr
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		handle, oerr := Open(dir)
		defer handle.Close()
		if oerr != nil {
			errs <- oerr
			return
		}
		<-start
		for range rounds {
			// Switch back and forth from a separate handle throughout.
			if _, _, _, cerr := handle.Checkout("other"); cerr != nil {
				errs <- cerr
				return
			}
			if _, _, _, cerr := handle.Checkout("master"); cerr != nil {
				// A repo initialised as main rather than master is fine; stop
				// rather than fail on the branch name.
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("checkout racing stage-and-commit: %v", err)
	}
}

// A create must never move an existing branch. SetReference overwrites a ref
// unconditionally, so a silent reset would discard the old tip with no
// pre-operation record of it anywhere.
func TestCreateBranchRejectsExisting(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	first, err := repo.Commit("first", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, _, _, err = repo.CreateBranch("feature", ""); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	// Move HEAD on so a reset would be observable.
	writeRepoFile(t, repo, "a.txt", "two\n")
	second, err := repo.Commit("second", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if first == second {
		t.Fatal("second commit did not advance HEAD; test cannot detect a reset")
	}

	_, _, _, err = repo.CreateBranch("feature", "")
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("re-create error = %v, want ErrBranchExists", err)
	}
	if !strings.Contains(err.Error(), first) {
		t.Errorf("error %q does not report the existing tip %s", err, first)
	}

	ref, err := repo.repo.Reference(plumbing.NewBranchReferenceName("feature"), false)
	if err != nil {
		t.Fatalf("Reference after refused create: %v", err)
	}
	if got := ref.Hash().String(); got != first {
		t.Errorf("branch tip = %s, want %s — the refused create moved it anyway", got, first)
	}
}

func TestCreateBranchRejectsInvalidName(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// git-check-ref-format rejects each of these.
	for _, name := range []string{"has space", "has..dots", "trailing.lock", "-leading-dash", "ends/", "back\\slash"} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := repo.CreateBranch(name, ""); err == nil {
				t.Errorf("CreateBranch(%q) succeeded, want a validation error", name)
			}
		})
	}
}

func TestCreateBranchFromStartPoint(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	first, err := repo.Commit("first", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeRepoFile(t, repo, "a.txt", "two\n")
	if _, err = repo.Commit("second", []string{"a.txt"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	sha, preOpSHA, _, err := repo.CreateBranch("from-first", first)
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if sha != first {
		t.Errorf("branch created at %s, want %s", sha, first)
	}
	if preOpSHA == "" {
		t.Error("preOpSHA empty, want the HEAD SHA at call time")
	}
	if _, _, _, err = repo.CreateBranch("bad-start", "no-such-rev"); err == nil {
		t.Error("CreateBranch with an unresolvable start point succeeded, want an error")
	}
}

// The existence check is only a check if nothing can create the branch between
// it and the ref write. Racing handles ask for the same name at different start
// points: exactly one must win, and the loser must be refused rather than
// silently resetting the winner's branch to its own start point.
func TestCreateBranchConcurrentSameNameDifferentStartPoints(t *testing.T) {
	dir := t.TempDir()
	seed, err := Init(dir)
	defer seed.Close()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeRepoFile(t, seed, "a.txt", "one\n")
	first, err := seed.Commit("first", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeRepoFile(t, seed, "a.txt", "two\n")
	second, err := seed.Commit("second", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if first == second {
		t.Fatal("commits share a SHA; the racers would be indistinguishable")
	}

	const racers = 8
	startPoints := []string{first, second}

	var wg sync.WaitGroup
	type outcome struct {
		sha string
		err error
	}
	results := make(chan outcome, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// A separate handle each, as every tool call gets.
			handle, oerr := Open(dir)
			defer handle.Close()
			if oerr != nil {
				results <- outcome{err: oerr}
				return
			}
			<-start
			sha, _, _, cerr := handle.CreateBranch("contended", startPoints[n%len(startPoints)])
			results <- outcome{sha: sha, err: cerr}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	var winners []string
	for res := range results {
		switch {
		case res.err == nil:
			winners = append(winners, res.sha)
		case errors.Is(res.err, ErrBranchExists):
			// The expected refusal for every loser.
		default:
			t.Errorf("unexpected error from a racing create: %v", res.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d creates succeeded, want exactly 1 — the check and the ref write are not atomic", len(winners))
	}

	// The surviving branch must sit at the winner's start point, untouched by
	// every refused create.
	ref, err := seed.repo.Reference(plumbing.NewBranchReferenceName("contended"), false)
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if got := ref.Hash().String(); got != winners[0] {
		t.Errorf("branch tip = %s, want the winning create's %s — a loser reset it", got, winners[0])
	}
}

// git records a commit in both logs — HEAD moved, and so did the branch it
// points at. Only the branch log was written before, so HEAD@{1}, which reads
// .git/logs/HEAD, did not resolve after a harness commit even though the method
// documented that undo.
func TestWorkspaceStageAndCommitWritesBothReflogs(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	if _, _, warn, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first"); err != nil || warn != nil {
		t.Fatalf("first commit: err=%v warn=%v", err, warn)
	}
	writeRepoFile(t, repo, "a.txt", "two\n")
	if _, _, warn, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "second"); err != nil || warn != nil {
		t.Fatalf("second commit: err=%v warn=%v", err, warn)
	}

	head, err := repo.repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	for _, ref := range []string{"HEAD", head.Name().String()} {
		got := reflogFile(t, repo, ref)
		if got == "" {
			t.Errorf("%s reflog is empty; a commit must appear in both logs", ref)
			continue
		}
		if !strings.Contains(got, "commit: second") {
			t.Errorf("%s reflog missing the commit entry:\n%s", ref, got)
		}
	}
	// Two commits, so HEAD@{1} has something to resolve to.
	if n := strings.Count(reflogFile(t, repo, "HEAD"), "commit: "); n != 2 {
		t.Errorf("HEAD reflog has %d commit entries, want 2 — HEAD@{1} would not resolve", n)
	}
}

// A detached HEAD has no branch to record, and HEAD must not be written twice
// for one move. Repository.Head() resolves the symbolic ref — it reports the
// branch name when attached but "HEAD" when detached — so deriving the branch
// from it appends HEAD's log twice, and HEAD@{1} then resolves to the new
// commit rather than the one before it.
func TestWorkspaceStageAndCommitDetachedHeadWritesHeadLogOnce(t *testing.T) {
	repo := newWorkspaceRepo(t)
	writeRepoFile(t, repo, "a.txt", "one\n")
	first, err := repo.Commit("first", []string{"a.txt"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Detach: point HEAD straight at the commit instead of at a branch.
	detached := plumbing.NewHashReference(plumbing.HEAD, plumbing.NewHash(first))
	if err = repo.repo.Storer.SetReference(detached); err != nil {
		t.Fatalf("SetReference: %v", err)
	}
	if raw, rerr := repo.repo.Reference(plumbing.HEAD, false); rerr != nil || raw.Type() == plumbing.SymbolicReference {
		t.Fatalf("HEAD is not detached (type %v, err %v); the test cannot detect the duplicate", raw.Type(), rerr)
	}

	writeRepoFile(t, repo, "a.txt", "two\n")
	newSHA, preOpSHA, warn, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "detached commit")
	if err != nil {
		t.Fatalf("detached commit: %v", err)
	}
	if warn != nil {
		t.Fatalf("unexpected warning: %v", warn)
	}
	if preOpSHA != first {
		t.Errorf("preOpSHA = %s, want %s", preOpSHA, first)
	}

	headLog := reflogFile(t, repo, "HEAD")
	if n := strings.Count(headLog, "commit: detached commit"); n != 1 {
		t.Errorf("HEAD reflog has %d entries for one commit, want 1 — HEAD@{1} would skip the pre-op commit:\n%s", n, headLog)
	}
	// The entry must still describe the real move.
	if !strings.Contains(headLog, first) || !strings.Contains(headLog, newSHA) {
		t.Errorf("HEAD reflog entry does not record %s → %s:\n%s", first, newSHA, headLog)
	}
}

// breakReflogs makes every reflog write fail by replacing .git/logs with a
// regular file, so the directory the entries need cannot be created.
func breakReflogs(t *testing.T, repo *Repo) {
	t.Helper()
	logs := filepath.Join(repo.path, ".git", "logs")
	if err := os.RemoveAll(logs); err != nil {
		t.Fatalf("RemoveAll logs: %v", err)
	}
	if err := os.WriteFile(logs, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("WriteFile logs: %v", err)
	}
}

// A reflog that cannot be written costs the convenience of HEAD@{1}, not the
// recovery path. The operation has already happened, so it must be reported as
// a warning rather than an error — telling the caller a write failed when it
// succeeded would be worse than the missing log entry.
func TestReflogFailureWarnsWithoutFailingTheWrite(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		repo := newWorkspaceRepo(t)
		writeRepoFile(t, repo, "a.txt", "one\n")
		if _, _, _, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "first"); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
		breakReflogs(t, repo)

		writeRepoFile(t, repo, "a.txt", "two\n")
		newSHA, _, warn, err := repo.WorkspaceStageAndCommit([]string{"a.txt"}, "second")
		if err != nil {
			t.Fatalf("commit reported failure for a reflog problem: %v", err)
		}
		if warn == nil {
			t.Fatal("no warning for an unwritable reflog")
		}
		if newSHA == "" {
			t.Error("no SHA returned; the commit itself must still have happened")
		}
		// The label says which log failed, not just that something did.
		if !strings.Contains(warn.Error(), "reflog") {
			t.Errorf("warning %q does not identify the reflog", warn)
		}
		// The commit is real: HEAD advanced to it.
		head, herr := repo.repo.Head()
		if herr != nil || head.Hash().String() != newSHA {
			t.Errorf("HEAD = %v (err %v), want the new commit %s", head, herr, newSHA)
		}
	})

	t.Run("branch", func(t *testing.T) {
		repo := newWorkspaceRepo(t)
		writeRepoFile(t, repo, "a.txt", "one\n")
		if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
		breakReflogs(t, repo)

		sha, _, warn, err := repo.CreateBranch("feature", "")
		if err != nil {
			t.Fatalf("CreateBranch reported failure for a reflog problem: %v", err)
		}
		if warn == nil {
			t.Fatal("no warning for an unwritable reflog")
		}
		if _, rerr := repo.repo.Reference(plumbing.NewBranchReferenceName("feature"), false); rerr != nil {
			t.Errorf("branch missing after a warned create: %v", rerr)
		}
		if sha == "" {
			t.Error("no SHA returned; the branch itself must still have been created")
		}
	})

	t.Run("checkout", func(t *testing.T) {
		repo := newWorkspaceRepo(t)
		writeRepoFile(t, repo, "a.txt", "one\n")
		if _, err := repo.Commit("first", []string{"a.txt"}); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
		if _, _, _, err := repo.CreateBranch("feature", ""); err != nil {
			t.Fatalf("CreateBranch: %v", err)
		}
		breakReflogs(t, repo)

		_, _, warn, err := repo.Checkout("feature")
		if err != nil {
			t.Fatalf("Checkout reported failure for a reflog problem: %v", err)
		}
		if warn == nil {
			t.Fatal("no warning for an unwritable reflog")
		}
		head, herr := repo.repo.Head()
		if herr != nil || head.Name().Short() != "feature" {
			t.Errorf("HEAD = %v (err %v), want the checkout to have happened", head, herr)
		}
	})
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
