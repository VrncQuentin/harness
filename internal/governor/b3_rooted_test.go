package governor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/VrncQuentin/harness/internal/tools"
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

const spillBait = "OUTSIDE-THE-SPILL-DIRECTORY"

// bigOutput returns a failure payload above the B3 threshold.
func bigOutput(marker string) string {
	return marker + strings.Repeat("x", b3Threshold+1)
}

// The spill directory itself being a link out of the cache is not an escape —
// it is the directory the harness was told to use. What must not happen is the
// spill writing *through* a pre-existing entry inside it. A hard link is the
// sharpest case: the entry is not a link the root could refuse, it is another
// name for a file elsewhere, so only publishing by rename leaves that file
// alone.
func TestApplyB3_DoesNotWriteThroughAHardLinkedSpillEntry(t *testing.T) {
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	dir := TooloutDir(cacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	bait := filepath.Join(base, "bait.txt")
	if err := os.WriteFile(bait, []byte(spillBait), 0o644); err != nil {
		t.Fatalf("WriteFile bait: %v", err)
	}

	spill := bigOutput("FULL-OUTPUT ")
	id := tooloutID("exec", spill)
	if err := os.Link(bait, filepath.Join(dir, id)); err != nil {
		t.Fatalf("hard links are expected to work here: %v", err)
	}

	g := New(nil, cacheDir)
	res := g.applyB3(context.Background(), "exec", tools.Result{
		Error:      "command failed",
		FullOutput: spill,
	})
	if !strings.Contains(res.Error, tools.TooloutScheme+id) {
		t.Fatalf("B3 did not emit a handle: %q", res.Error)
	}

	got, err := os.ReadFile(bait)
	if err != nil {
		t.Fatalf("ReadFile bait: %v", err)
	}
	if string(got) != spillBait {
		t.Fatalf("the spill wrote through the hard link and changed a file outside the spill directory: %q", got)
	}
	// And the spill itself landed, complete, under its own name.
	written, err := os.ReadFile(filepath.Join(dir, id))
	if err != nil {
		t.Fatalf("ReadFile spill: %v", err)
	}
	if string(written) != spill {
		t.Fatalf("the spill file does not hold the full output (%d bytes, want %d)", len(written), len(spill))
	}
}

// A spill id that is a directory link out of the spill directory must not be
// followed. os.Root refuses an absolute link target outright, which is what a
// Windows junction always stores, so this runs on both platforms.
func TestApplyB3_DoesNotFollowALinkedSpillDirectoryEntry(t *testing.T) {
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	dir := TooloutDir(cacheDir)
	outside := filepath.Join(base, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	spill := bigOutput("FULL-OUTPUT ")
	id := tooloutID("exec", spill)
	// The id names a directory that leads outside; a write that resolved it
	// would land in the attacker's directory.
	mustLinkDir(t, outside, filepath.Join(dir, id))

	g := New(nil, cacheDir)
	_ = g.applyB3(context.Background(), "exec", tools.Result{
		Error:      "command failed",
		FullOutput: spill,
	})

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the spill wrote %d entries outside the spill directory: %v", len(entries), entries)
	}
}

// A reader must never observe a partially copied spill. Publication is by
// rename, so the name either does not exist or holds the whole output.
func TestApplyB3_ConcurrentSpillsArePublishedWhole(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g := New(nil, cacheDir)

	const writers = 6
	spill := bigOutput("FULL-OUTPUT ")
	id := tooloutID("exec", spill)

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.applyB3(context.Background(), "exec", tools.Result{
				Error:      "command failed",
				FullOutput: spill,
			})
		}()
	}
	// A reader racing the writers sees either nothing or the complete file.
	done := make(chan struct{})
	go func() {
		defer close(done)
		path := filepath.Join(TooloutDir(cacheDir), id)
		for range 200 {
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if len(body) != 0 && string(body) != spill {
				t.Errorf("a reader observed a partial spill: %d bytes, want 0 or %d", len(body), len(spill))
				return
			}
		}
	}()
	wg.Wait()
	<-done

	body, err := os.ReadFile(filepath.Join(TooloutDir(cacheDir), id))
	if err != nil {
		t.Fatalf("ReadFile spill: %v", err)
	}
	if string(body) != spill {
		t.Fatalf("final spill is %d bytes, want %d", len(body), len(spill))
	}
	// No temporary files survive a race between equal writers.
	entries, err := os.ReadDir(TooloutDir(cacheDir))
	if err != nil {
		t.Fatalf("ReadDir spill dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".harness-write-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}
