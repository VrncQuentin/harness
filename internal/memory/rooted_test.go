package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// escapeFixture is a memory repo with a directory inside it that is really a
// link to a directory outside it, plus a file outside that must stay untouched.
type escapeFixture struct {
	repo    *DirReader
	root    string
	outside string
	// bait is the absolute path of a file outside the repo that every test
	// here tries, and must fail, to reach.
	bait string
}

const baitBody = "OUTSIDE-THE-MEMORY-REPO"

// escapeLinkName is the repo-relative directory every escape fixture plants as
// a link to the outside directory.
const escapeLinkName = "linked"

// newEscapeFixture builds repo/linked as a link to an outside directory holding
// bait.txt. Everything a test does afterwards addresses the repo through the
// reader, never through root.
func newEscapeFixture(t *testing.T) escapeFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	bait := filepath.Join(outside, "bait.txt")
	if err := os.WriteFile(bait, []byte(baitBody), 0o644); err != nil {
		t.Fatalf("WriteFile bait: %v", err)
	}
	mustLinkDir(t, outside, filepath.Join(root, escapeLinkName))

	repo, err := OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return escapeFixture{repo: repo, root: root, outside: outside, bait: bait}
}

// assertBaitIntact reports whether the file outside the repo still holds
// exactly what it started with. Every mutating test calls it: refusing the
// operation is one acceptable outcome and performing it inside the repo is
// another, but changing something outside is never one.
func (f escapeFixture) assertBaitIntact(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(f.bait)
	if err != nil {
		t.Fatalf("the file outside the repo was removed or made unreadable: %v", err)
	}
	if string(got) != baitBody {
		t.Fatalf("the file outside the repo was modified: got %q, want %q", got, baitBody)
	}
}

// A link in the middle of a path is the case lexical validation cannot see:
// "linked/bait.txt" contains no "..", is not absolute, and passes checkRel
// unchanged. Only resolving it through the pinned repo refuses it.
func TestDirReader_IntermediateLinkCannotBeRead(t *testing.T) {
	f := newEscapeFixture(t)

	got, err := f.repo.Read("linked/bait.txt")
	if err == nil {
		t.Fatalf("read through a link out of the repo returned %q", got)
	}
	if strings.Contains(string(got), baitBody) {
		t.Fatalf("content from outside the repo was returned: %q", got)
	}
	f.assertBaitIntact(t)
}

// Every mutating entry point has to refuse the same shape. A reader that
// refuses reads but writes through the link is not contained.
func TestDirReader_IntermediateLinkCannotBeWrittenThrough(t *testing.T) {
	tests := []struct {
		name string
		call func(*DirReader) error
	}{
		{
			name: "WriteFile",
			call: func(r *DirReader) error { return r.WriteFile("linked/bait.txt", []byte("clobbered")) },
		},
		{
			name: "AppendFile",
			call: func(r *DirReader) error { return r.AppendFile("linked/bait.txt", []byte("appended")) },
		},
		{
			name: "MkdirAll",
			call: func(r *DirReader) error { return r.MkdirAll("linked/newdir") },
		},
		{
			name: "RemoveAll",
			call: func(r *DirReader) error { return r.RemoveAll("linked/bait.txt") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEscapeFixture(t)
			if err := tt.call(f.repo); err == nil {
				t.Errorf("%s through a link out of the repo was accepted", tt.name)
			}
			f.assertBaitIntact(t)
			if _, err := os.Stat(filepath.Join(f.outside, "newdir")); err == nil {
				t.Error("a directory was created outside the repo")
			}
		})
	}
}

// Listing has to be contained too. A ListDirs or Glob that follows the link
// discloses the names of files outside the repo even if it never reads one.
func TestDirReader_IntermediateLinkCannotBeEnumerated(t *testing.T) {
	f := newEscapeFixture(t)

	if dirs, err := f.repo.ListDirs("linked"); err == nil && len(dirs) > 0 {
		t.Errorf("ListDirs enumerated outside the repo: %v", dirs)
	}
	matches, err := f.repo.Glob("linked/*.txt")
	if err == nil && len(matches) > 0 {
		t.Errorf("Glob matched outside the repo: %v", matches)
	}
	entries, err := f.repo.Walk("")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Path, "bait") {
			t.Errorf("Walk reported an entry from outside the repo: %s", e.Path)
		}
	}
	f.assertBaitIntact(t)
}

// The leaf case: a repo entry that is a hard link to a file outside the repo.
//
// A hard link is the sharpest version of this and the one that runs everywhere
// — NTFS supports it without the privilege a file symlink needs on Windows, so
// there is no platform where this invariant goes unchecked. It is also the case
// no containment check can catch: the entry is not a link the root can refuse,
// it *is* the outside file, under a second name. Only publishing by rename
// saves it, because a rename replaces the directory entry and leaves the inode
// that held the name alone.
func TestDirReader_WriteFileReplacesHardLinkedLeafInsteadOfWritingThroughIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bait := filepath.Join(base, "bait.txt")
	if err := os.WriteFile(bait, []byte(baitBody), 0o644); err != nil {
		t.Fatalf("WriteFile bait: %v", err)
	}
	if err := os.Link(bait, filepath.Join(root, "facts.md")); err != nil {
		t.Fatalf("hard links are expected to work here: %v", err)
	}

	repo, err := OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = repo.Close() }()

	if err := repo.WriteFile("facts.md", []byte("promoted fact\n")); err != nil {
		t.Fatalf("WriteFile over a hard-linked leaf: %v", err)
	}
	got, err := os.ReadFile(bait)
	if err != nil {
		t.Fatalf("ReadFile bait: %v", err)
	}
	if string(got) != baitBody {
		t.Fatalf("the write went through the hard link and changed a file outside the repo: %q", got)
	}
	inRepo, err := repo.Read("facts.md")
	if err != nil {
		t.Fatalf("Read after WriteFile: %v", err)
	}
	if string(inRepo) != "promoted fact\n" {
		t.Fatalf("the new content did not land in the repo: %q", inRepo)
	}
}

// The same leaf case with a symbolic link, which the root refuses outright
// rather than replacing.
//
// Creating a file symlink on Windows needs Developer Mode or an elevated
// process, so this skips there. The invariant is not left uncovered: the
// hard-link test above is stronger and runs on both platforms, and the
// intermediate-link tests use a junction, which needs no privilege, so
// containment through a link is exercised on Windows too.
func TestDirReader_WriteFileRefusesSymlinkedLeafOutsideRepo(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bait := filepath.Join(base, "bait.txt")
	if err := os.WriteFile(bait, []byte(baitBody), 0o644); err != nil {
		t.Fatalf("WriteFile bait: %v", err)
	}
	if err := os.Symlink(bait, filepath.Join(root, "facts.md")); err != nil {
		t.Skipf("file symlinks need a privilege this environment lacks: %v", err)
	}

	repo, err := OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = repo.Close() }()

	_ = repo.WriteFile("facts.md", []byte("promoted fact\n"))
	got, err := os.ReadFile(bait)
	if err != nil {
		t.Fatalf("ReadFile bait: %v", err)
	}
	if string(got) != baitBody {
		t.Fatalf("the write followed the symlink and changed a file outside the repo: %q", got)
	}
}

// AppendFile is the session log's primitive, and it is the one operation that
// genuinely cannot be saved from a hard link: appending to an inode reached by
// a second name appends to that inode, and no containment check can see the
// difference because there is no link to refuse. What it must still refuse is
// reaching an outside file *through a directory link*, which is the shape an
// attacker can actually plant inside a repo the harness scaffolds.
func TestDirReader_AppendFileCannotEscapeThroughLinkedDirectory(t *testing.T) {
	f := newEscapeFixture(t)

	if err := f.repo.AppendFile("linked/bait.txt", []byte("{\"id\":\"x\"}\n")); err == nil {
		t.Error("append through a directory link out of the repo was accepted")
	}
	f.assertBaitIntact(t)
}

// The reader is pinned, so the repo's *name* no longer selects what it reaches.
// Re-pointing the name at another directory after the pin must not redirect it.
func TestDirReader_RepoRenamedAfterPinKeepsReachingTheSameDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	evil := filepath.Join(base, "evil")
	for _, dir := range []string{real, evil} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, "rules.md"), []byte("the genuine rules"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	const secret = "SECRET-FROM-THE-REPLACEMENT-REPO"
	if err := os.WriteFile(filepath.Join(evil, "rules.md"), []byte(secret), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	name := filepath.Join(base, "repo")
	mustLinkDir(t, real, name)

	repo, err := OpenDirReader(name)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = repo.Close() }()

	// Re-point the configured name at the attacker's directory, after the pin.
	if err := os.Remove(name); err != nil {
		t.Skipf("cannot re-point the repo name here: %v", err)
	}
	mustLinkDir(t, evil, name)

	got, err := repo.Read("rules.md")
	if err != nil {
		t.Fatalf("Read after the repo name was re-pointed: %v", err)
	}
	if strings.Contains(string(got), secret) {
		t.Fatalf("the reader followed the name to the replacement directory: %q", got)
	}
}

// Two spellings of one repo must not become two repos. On Windows that means
// case; everywhere it means a link.
func TestDirReader_AliasedRootReachesTheSameFiles(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "repo")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	alias := filepath.Join(base, "alias")
	mustLinkDir(t, real, alias)

	viaReal, err := OpenDirReader(real)
	if err != nil {
		t.Fatalf("OpenDirReader real: %v", err)
	}
	defer func() { _ = viaReal.Close() }()
	viaAlias, err := OpenDirReader(alias)
	if err != nil {
		t.Fatalf("OpenDirReader alias: %v", err)
	}
	defer func() { _ = viaAlias.Close() }()

	if err := viaReal.WriteFile("facts.md", []byte("written through the real name\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := viaAlias.Read("facts.md")
	if err != nil {
		t.Fatalf("Read through the alias: %v", err)
	}
	if string(got) != "written through the real name\n" {
		t.Fatalf("the alias did not see the write: %q", got)
	}
}

// RemoveAll must never be talked into deleting the repo itself, whatever
// spelling of "here" it is handed.
func TestDirReader_RemoveAllRefusesTheRepoRoot(t *testing.T) {
	for _, rel := range []string{"", ".", "./", "./."} {
		t.Run("rel="+subtestName(rel), func(t *testing.T) {
			r, root := newTestRepoAt(t, map[string]string{"facts.md": "keep me"})
			if err := r.RemoveAll(rel); err == nil {
				t.Errorf("RemoveAll(%q) was accepted", rel)
			}
			if _, err := os.Stat(filepath.Join(root, "facts.md")); err != nil {
				t.Errorf("repo contents were removed: %v", err)
			}
		})
	}
}

// subtestName renders an empty string visibly in a subtest name.
func subtestName(s string) string {
	if s == "" {
		return "empty"
	}
	return s
}

// Concurrent appends must interleave by record, not by byte. The pinned root's
// append opens O_APPEND, so the kernel owns the position.
func TestDirReader_ConcurrentAppendsKeepWholeRecords(t *testing.T) {
	repo, root := newTestRepoAt(t, nil)

	const writers = 8
	const perWriter = 20
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			line := []byte(strings.Repeat(string(rune('a'+w)), 64) + "\n")
			for range perWriter {
				if err := repo.AppendFile("sessions.jsonl", line); err != nil {
					t.Errorf("AppendFile: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	body, err := os.ReadFile(filepath.Join(root, "sessions.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("line count = %d, want %d", len(lines), writers*perWriter)
	}
	for i, line := range lines {
		if len(line) != 64 {
			t.Fatalf("line %d is %d bytes, want 64 — an append was interleaved mid-record: %q", i, len(line), line)
		}
		if strings.Count(line, line[:1]) != 64 {
			t.Fatalf("line %d mixes two writers' bytes: %q", i, line)
		}
	}
}

// A missing file still reads as fs.ErrNotExist through the pinned root, which
// is what lets the session log treat "no sessions yet" as an empty list rather
// than a failure.
func TestDirReader_MissingFileStillReportsNotExist(t *testing.T) {
	r := newTestRepo(t, nil)
	_, err := r.Read("sessions.jsonl")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// Walk sorts and prunes .git exactly as it did before the migration; the
// traversal changed, the contract did not.
func TestDirReader_WalkOrderingAndGitPruningUnchanged(t *testing.T) {
	r := newTestRepo(t, map[string]string{
		"zeta.md":                 "z",
		"alpha.md":                "a",
		"agents/coder/notes.md":   "n",
		"agents/beta/persona.md":  "p",
		".git/HEAD":               "ref: refs/heads/main",
		".git/objects/ab/cd":      "x",
		"episodes/coder/01.md":    "e",
		"index/_episodes/dat.bin": "v",
	})
	entries, err := r.Walk("")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Path, ".git") {
			t.Fatalf("Walk leaked git plumbing: %s", e.Path)
		}
		paths = append(paths, e.Path)
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] >= paths[i] {
			t.Fatalf("Walk is not sorted: %q before %q", paths[i-1], paths[i])
		}
	}
}
