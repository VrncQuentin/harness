package session

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
)

// sessionLinkDir creates a directory link at link pointing at target, preferring
// a symlink and falling back to a Windows junction. Junctions need no privilege
// and are traversed exactly like symlinks, so they exercise the same escape on
// machines where symlink creation is denied.
func sessionLinkDir(t *testing.T, target, link string) {
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

// sessionLinkFile creates a file symlink at link pointing at target. File-level
// symlinks need Developer Mode on Windows, so the test skips where they cannot
// be created.
func sessionLinkFile(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("file symlinks require Developer Mode on Windows")
		}
		t.Fatal(err)
	}
}

func writeOutsideLog(t *testing.T, dir string, id string) {
	t.Helper()
	rec := Record{ID: id, Agent: "coder", Project: "global"}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.jsonl"), append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSessionLog_ReadsThroughPinnedRoot is the finding 7.1 discriminator:
// ReadAll must read sessions.jsonl through the pinned root, not by pathname.
// A sessions.jsonl name inside the repo that links out of it must not hand the
// outside log's records to the caller — a pathname open would follow the link
// and return them.
func TestSessionLog_ReadsThroughPinnedRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOutsideLog(t, outside, "outside-record")
	sessionLinkFile(t, filepath.Join(outside, "sessions.jsonl"), filepath.Join(repoRoot, "sessions.jsonl"))

	reader, err := memory.NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	got, err := ReadAll(reader, sessionsLogRel)
	if err == nil && len(got) > 0 {
		t.Fatalf("ReadAll followed the escaping link and read %d record(s)", len(got))
	}
	if err == nil && len(got) == 0 {
		t.Fatal("ReadAll returned empty for an escaping link instead of failing closed")
	}
}

// TestSessionLog_AppendsThroughPinnedRoot is the finding 7.2 discriminator:
// AppendRecord must append through the pinned root, not by pathname. A reader
// opened through a stable alias keeps writing to the directory it pinned even
// after the alias spelling is repointed at another directory — and a repointed
// spelling fails closed instead of silently appending into the replacement.
func TestSessionLog_AppendsThroughPinnedRoot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	evil := filepath.Join(dir, "evil")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	sessionLinkDir(t, real, alias)

	reader, err := memory.NewDirReader(alias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	rec := Record{ID: "pinned", Agent: "coder", Project: "global"}
	if err := AppendRecord(reader, sessionsLogRel, rec); err != nil {
		t.Fatalf("AppendRecord through a stable alias: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "sessions.jsonl")); err != nil {
		t.Fatalf("append did not land in the pinned directory: %v", err)
	}

	// Repoint the alias at a different directory. The reader stays pinned to
	// the original, so the next append must fail closed rather than follow the
	// repointed spelling into evil.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	sessionLinkDir(t, evil, alias)

	if err := AppendRecord(reader, sessionsLogRel, rec); err == nil {
		t.Fatal("append followed a repointed alias instead of the pinned root")
	}
	if _, err := os.Stat(filepath.Join(evil, "sessions.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("append escaped into the repointed alias target: %v", err)
	}
}

// TestSessionLog_AppendDoesNotFollowLinkOutOfRoot is the finding 7.3
// discriminator: an append must not open sessions.jsonl by pathname, so a
// sessions.jsonl name that links out of the repo is refused and the outside
// file is left unchanged.
func TestSessionLog_AppendDoesNotFollowLinkOutOfRoot(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	const outsideBody = "outsider\n"
	if err := os.WriteFile(filepath.Join(outside, "sessions.jsonl"), []byte(outsideBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionLinkFile(t, filepath.Join(outside, "sessions.jsonl"), filepath.Join(repoRoot, "sessions.jsonl"))

	reader, err := memory.NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	rec := Record{ID: "inner", Agent: "coder", Project: "global"}
	err = AppendRecord(reader, sessionsLogRel, rec)
	if err == nil {
		t.Fatal("append followed a link out of the root")
	}
	got, err := os.ReadFile(filepath.Join(outside, "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != outsideBody {
		t.Errorf("linked-outside file was modified: got %q, want %q", string(got), outsideBody)
	}
}

// countingWriter wraps a rooted FileWriter and counts calls so a test can
// assert which artifact writes were attempted and in what order.
type countingWriter struct {
	real  FileWriter
	calls int
}

func (w *countingWriter) WriteFile(relPath string, data []byte) error {
	w.calls++
	return w.real.WriteFile(relPath, data)
}

// TestSessionLog_SidecarPublishedThroughPinnedRoot is the finding 7.4
// discriminator: sidecar publication goes through the pinned memory writer
// (m.deps.Writer.WriteFile), so when the sidecar's episode directory is a
// link out of the repo the sidecar write itself must fail closed and write
// nothing outside. Under PR 11 the sidecar is the first artifact Save
// publishes, so a link installed ahead of Save is exactly the write that hits
// it; a failed sidecar must also emit no pending recovery record.
func TestSessionLog_SidecarPublishedThroughPinnedRoot(t *testing.T) {
	fi := newFakeInference(summaryTokens("sidecar summary"))
	root, repo := scaffoldMemoryRepo(t, "coder")
	reader, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	outside := t.TempDir()
	// Replace the episodes tree with a link out of the root before Save.
	episodeDir := filepath.Join(root, "episodes")
	if err := os.RemoveAll(episodeDir); err != nil {
		t.Fatalf("remove episode dir: %v", err)
	}
	sessionLinkDir(t, outside, episodeDir)

	writer := &countingWriter{real: reader}
	mgr, err := NewManager(ManagerDeps{
		Repo:             repo,
		Writer:           writer,
		Reader:           reader,
		Appender:         reader,
		Inference:        fi,
		SummarizerPrompt: func() string { return "test" },
	}, project.GlobalSlug)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	s := mgr.Start("coder")
	if err := mgr.Append(s.ID, inference.Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	_, err = mgr.Save(context.Background(), s.ID)
	if err == nil {
		t.Fatal("Save published a sidecar through an escaping link")
	}
	// The failure must come from the sidecar publication, which is the first
	// and only artifact write attempted.
	if !strings.Contains(err.Error(), s.ID+episodeSidecarSuffix) {
		t.Fatalf("error should come from sidecar publication, got %v", err)
	}
	if writer.calls != 1 {
		t.Fatalf("sidecar was not the only write attempted: %d WriteFile calls, want 1", writer.calls)
	}
	// A failed sidecar must emit no pending recovery record.
	records, err := ReadAll(reader, sessionsLogRel)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("sidecar failure emitted %d log records, want none", len(records))
	}
	// Nothing may have landed outside: no episode, no sidecar.
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("episode/sidecar escaped the root into %s: %v", outside, entries)
	}
}

// stubLogReader returns canned bytes or a canned error so the 7.5 discriminator
// can stage real read failures without touching the filesystem.
type stubLogReader struct {
	data []byte
	err  error
}

func (s stubLogReader) Read(string) ([]byte, error) { return s.data, s.err }

// TestSessionLog_ReadAllOnlyErrNotExistMeansNoSessions is the finding 7.5
// discriminator: only fs.ErrNotExist means "no sessions". A permission,
// containment, or I/O failure must propagate, not be treated as an empty log.
func TestSessionLog_ReadAllOnlyErrNotExistMeansNoSessions(t *testing.T) {
	// fs.ErrNotExist is the one error that means no sessions.
	if _, err := ReadAll(stubLogReader{err: fs.ErrNotExist}, sessionsLogRel); err != nil {
		t.Fatalf("fs.ErrNotExist must mean no sessions, got %v", err)
	}

	// A permission failure propagates.
	permErr := &fs.PathError{Op: "open", Path: sessionsLogRel, Err: fs.ErrPermission}
	if _, err := ReadAll(stubLogReader{err: permErr}, sessionsLogRel); err == nil {
		t.Fatal("permission failure was swallowed as 'no sessions'")
	} else if !errors.Is(err, permErr) {
		t.Fatalf("permission failure should propagate, got %v", err)
	}

	// An I/O failure propagates.
	ioErr := errors.New("disk read error")
	if _, err := ReadAll(stubLogReader{err: ioErr}, sessionsLogRel); !errors.Is(err, ioErr) {
		t.Fatalf("I/O failure should propagate, got %v", err)
	}

	// A real reader on a missing log returns empty, not an error.
	reader, err := memory.NewDirReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if got, err := ReadAll(reader, sessionsLogRel); err != nil || len(got) != 0 {
		t.Fatalf("missing log on a real reader: %d records, err %v", len(got), err)
	}

	// A containment failure (escaping link) must propagate, not read as empty.
	t.Run("containment failure propagates", func(t *testing.T) {
		dir := t.TempDir()
		repoRoot := filepath.Join(dir, "repo")
		outside := filepath.Join(dir, "outside")
		if err := os.MkdirAll(repoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		writeOutsideLog(t, outside, "outside-record")
		sessionLinkFile(t, filepath.Join(outside, "sessions.jsonl"), filepath.Join(repoRoot, "sessions.jsonl"))

		reader, err := memory.NewDirReader(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reader.Close() })
		if _, err := ReadAll(reader, sessionsLogRel); err == nil {
			t.Fatal("containment failure was treated as 'no sessions'")
		}
	})
}
