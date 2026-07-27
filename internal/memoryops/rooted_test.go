package memoryops

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/VrncQuentin/harness/internal/memory"
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

// The episode index used to be located by an absolute "<repo>/index/_episodes"
// pathname, pinned on its own. That is exactly as safe as the name it was
// handed: with "index" a link to somewhere else, the pin succeeds, the
// directory is genuinely a directory, and every vector the harness writes lands
// outside the project memory repo.
//
// Resolving the location through the repo's own handle refuses it instead.
func TestEpisodeIndex_LinkedIndexDirectoryCannotEscapeTheRepo(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	mustLinkDir(t, outside, filepath.Join(root, "index"))

	mem, err := memory.OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = mem.Close() }()

	idx, err := NewEpisodeIndex(mem, EpisodeIndexRootRel)
	if err == nil {
		// If the index did open, nothing may have been written outside.
		_ = idx.Upsert("episodes/coder/one", "hash", [][]float32{{1, 0}})
		_ = idx.Close()
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the index wrote %d entries outside the repository: %v", len(entries), entries)
	}
}

// Re-pointing the index directory's name after it has been pinned must not
// redirect the index. The handle refers to the directory, not the name.
//
// Whether the write then succeeds is a platform question and deliberately not
// asserted: Linux keeps the pinned directory usable after its name is taken
// away, while Windows makes writes through a handle on a removed directory fail
// with a permission error. Both are acceptable. Writing into the attacker's
// directory is not, and that is what is checked.
func TestEpisodeIndex_IndexDirectoryRepointAfterPinIsNotFollowed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "index"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	mem, err := memory.OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = mem.Close() }()

	idx, err := NewEpisodeIndex(mem, EpisodeIndexRootRel)
	if err != nil {
		t.Fatalf("NewEpisodeIndex: %v", err)
	}
	defer func() { _ = idx.Close() }()

	// Re-point _episodes at a directory outside the repo, after the pin.
	pinned := filepath.Join(root, "index", "_episodes")
	if err := os.Remove(pinned); err != nil {
		t.Skipf("cannot replace the pinned index directory here: %v", err)
	}
	mustLinkDir(t, outside, pinned)

	upsertErr := idx.Upsert("episodes/coder/one", "hash", [][]float32{{1, 0}})

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the index followed the re-pointed name and wrote outside: %v", entries)
	}
	if upsertErr != nil {
		// The pinned directory is gone; refusing is the correct outcome and
		// the disclosure check above has already passed.
		return
	}
	results, err := idx.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want the one upserted vector", results)
	}
}

// Concurrent upserts must not interleave into a corrupt vector file. Each one
// measures, appends, and records an offset under the index mutex and through a
// single file handle; if any of that were reopened per step the manifest would
// end up describing overlapping ranges.
func TestEpisodeIndex_ConcurrentUpsertsKeepTheManifestConsistent(t *testing.T) {
	root := t.TempDir()
	mem, err := memory.OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader: %v", err)
	}
	defer func() { _ = mem.Close() }()

	idx, err := NewEpisodeIndex(mem, EpisodeIndexRootRel)
	if err != nil {
		t.Fatalf("NewEpisodeIndex: %v", err)
	}
	defer func() { _ = idx.Close() }()

	// One serial upsert first so the index exists before the racers start;
	// lazy creation is a separate concern from concurrent appends.
	if err := idx.Upsert("episodes/coder/seed", "seed", [][]float32{{1, 0}}); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			source := "episodes/coder/" + string(rune('a'+w))
			if err := idx.Upsert(source, "hash", [][]float32{{float32(w), 1}}); err != nil {
				t.Errorf("Upsert %s: %v", source, err)
			}
		}(w)
	}
	wg.Wait()

	// Re-opening validates the manifest against the vector file: overlapping or
	// past-the-end offsets are rejected there, so a clean re-open is the proof.
	reopened, err := NewEpisodeIndex(mem, EpisodeIndexRootRel)
	if err != nil {
		t.Fatalf("re-open after concurrent upserts: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	for w := range writers {
		source := "episodes/coder/" + string(rune('a'+w))
		if !reopened.Contains(source) {
			t.Errorf("%s is missing from the persisted manifest", source)
		}
	}
}
