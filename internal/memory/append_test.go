package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirReader_AppendFile_CreatesNew(t *testing.T) {
	r := newTestRepo(t, nil)
	if err := r.AppendFile("sessions.jsonl", []byte("record-one\n")); err != nil {
		t.Fatalf("AppendFile: %v", err)
	}
	got, err := r.Read("sessions.jsonl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "record-one\n" {
		t.Errorf("AppendFile = %q, want %q", string(got), "record-one\n")
	}
}

func TestDirReader_AppendFile_PreservesExisting(t *testing.T) {
	r := newTestRepo(t, map[string]string{"sessions.jsonl": "existing\n"})
	if err := r.AppendFile("sessions.jsonl", []byte("record-one\n")); err != nil {
		t.Fatalf("AppendFile: %v", err)
	}
	got, err := r.Read("sessions.jsonl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "existing\nrecord-one\n" {
		t.Errorf("AppendFile clobbered the log: got %q, want %q", string(got), "existing\nrecord-one\n")
	}
}

func TestDirReader_AppendFile_CreatesMissingParentsThroughRoot(t *testing.T) {
	dir := t.TempDir()
	r, err := NewDirReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	if err := r.AppendFile("episodes/coder/sessions.jsonl", []byte("record\n")); err != nil {
		t.Fatalf("AppendFile: %v", err)
	}
	// The parents must exist inside the pinned root.
	for _, rel := range []string{"episodes", "episodes/coder"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing parent %s created through root: %v", rel, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "episodes", "coder", "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "record\n" {
		t.Errorf("AppendFile = %q, want %q", string(got), "record\n")
	}
}

func TestDirReader_AppendFile_RejectsBadPaths(t *testing.T) {
	r := newTestRepo(t, nil)
	tests := []string{
		"",
		"../outside",
		"episodes/../../etc/passwd",
		"episodes\\..\\..\\etc",
		"/etc/passwd",
		"C:/windows/system32",
		"C:\\windows\\system32",
	}
	for _, tc := range tests {
		t.Run(tc, func(t *testing.T) {
			if err := r.AppendFile(tc, []byte("x")); err == nil {
				t.Errorf("AppendFile(%q): expected error, got nil", tc)
			}
		})
	}
}

func TestDirReader_AppendFile_RejectsRepoRoot(t *testing.T) {
	r, err := NewDirReader(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.AppendFile(".", []byte("x")); err == nil {
		t.Error("AppendFile('.') should be rejected")
	}
	if err := r.AppendFile("./", []byte("x")); err == nil {
		t.Error("AppendFile('./') should be rejected")
	}
}

func TestDirReader_AppendFile_RefusesLinkOutOfRoot(t *testing.T) {
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
	if err := os.Symlink(filepath.Join(outside, "sessions.jsonl"), filepath.Join(repoRoot, "sessions.jsonl")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("file symlinks require Developer Mode on Windows")
		}
		t.Fatal(err)
	}
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	if err := r.AppendFile("sessions.jsonl", []byte("record\n")); err == nil {
		t.Fatal("AppendFile followed a link out of the root")
	}
	got, err := os.ReadFile(filepath.Join(outside, "sessions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != outsideBody {
		t.Errorf("linked-outside file was modified: got %q, want %q", string(got), outsideBody)
	}
}

// A root-level failure must propagate: an append into a missing parent that
// the rooted MkdirAll cannot reach surfaces as an error the caller sees, not
// a silent no-op.
func TestDirReader_AppendFile_MissingParentErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	repoRoot := filepath.Join(dir, "repo")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	mustLinkDir(t, outside, filepath.Join(repoRoot, "leak"))
	r, err := NewDirReader(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	err = r.AppendFile("leak/sub/sessions.jsonl", []byte("record\n"))
	if err == nil {
		t.Fatal("append through an escaping parent should fail")
	}
	// The outside tree must not have gained the append.
	if _, statErr := os.Stat(filepath.Join(outside, "sub")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("append escaped the root: outside/sub exists: %v", statErr)
	}
}
