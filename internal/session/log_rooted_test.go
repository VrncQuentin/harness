package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// mustLinkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction, which needs no privilege and
// is traversed the same way. The test is skipped when neither is available.
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

// The session log is the harness's own audit trail. Appending to it by joining
// an absolute repo path meant a link anywhere along that path sent the records
// out of the repository; going through the pinned repo refuses it.
func TestAppendRecord_CannotEscapeThroughALinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	mustLinkDir(t, outside, filepath.Join(root, "linked"))

	repo := openTestRepo(t, root)
	rec := Record{ID: "2026-04-26T22-15-03Z", Agent: "coder", SavedAt: time.Now().UTC()}
	if err := AppendRecord(repo, "linked/sessions.jsonl", rec); err == nil {
		t.Error("appending through a link out of the repo was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("session records were written outside the repo: %v", entries)
	}
}

// Reading the log has to be contained for the same reason writing does: the
// resume picker renders what it finds, so a read that follows a link out of the
// repo discloses whatever is there.
func TestReadAll_CannotEscapeThroughALinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "sessions.jsonl"),
		[]byte(`{"id":"SECRET-SESSION","agent":"coder"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustLinkDir(t, outside, filepath.Join(root, "linked"))

	repo := openTestRepo(t, root)
	// A refusal is an error rather than an empty result, and deliberately so:
	// only fs.ErrNotExist means "no sessions yet". A containment refusal that
	// read as an empty log would hide a repo pointing somewhere unexpected.
	got, err := ReadAll(repo, "linked/sessions.jsonl")
	if err == nil && len(got) == 0 {
		t.Fatal("a refused read was reported as an empty log")
	}
	for _, r := range got {
		if strings.Contains(r.ID, "SECRET-SESSION") {
			t.Fatalf("the read followed the link and returned a record from outside the repo: %+v", r)
		}
	}
}

// A missing log still reads as "no sessions yet" rather than an error. This is
// the behaviour the resume picker depends on and the migration had to preserve.
func TestReadAll_MissingLogThroughPinnedRepoIsEmpty(t *testing.T) {
	repo := openTestRepo(t, t.TempDir())
	got, err := ReadAll(repo, "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll on a missing log: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records for a missing log, want 0", len(got))
	}
}

// Every saved record has to survive a concurrent save of another session. The
// append is O_APPEND through the pinned repo, so two writers extend the file
// rather than overwriting each other's line.
func TestAppendRecord_ConcurrentSavesKeepEveryRecord(t *testing.T) {
	dir := t.TempDir()
	repo := openTestRepo(t, dir)

	const writers = 8
	const perWriter = 10
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				rec := Record{
					ID:      string(rune('a'+w)) + "-" + string(rune('0'+i)),
					Agent:   "coder",
					SavedAt: time.Now().UTC(),
					SaveSeq: i + 1,
				}
				if err := AppendRecord(repo, "sessions.jsonl", rec); err != nil {
					t.Errorf("AppendRecord: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	got, err := ReadAll(repo, "sessions.jsonl")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("records = %d, want %d — a concurrent append was lost or garbled", len(got), writers*perWriter)
	}
}
