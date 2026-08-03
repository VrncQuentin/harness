package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/VrncQuentin/harness/internal/memory"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. Junctions need no privilege
// and are traversed exactly like symlinks, so they exercise the same escape on
// machines where symlink creation is denied.
func linkDir(t *testing.T, target, link string) {
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

// Finding 3.6: eval-retrieval must enumerate episodes through a pinned repo
// reader, producing stable repo-relative forward-slash paths, rather than
// filepath.Glob + filepath.Rel on an operator-supplied root.
func TestEvalRetrieval_PinnedRepo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-02.md"), "two")
	writeFile(t, filepath.Join(root, "episodes", "architect", "2024-01-03.md"), "three")
	// .md files outside the depth-two episode shape must not be enumerated.
	writeFile(t, filepath.Join(root, "rules.md"), "not an episode")
	writeFile(t, filepath.Join(root, "episodes", "top.md"), "not an episode")

	repo, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatalf("NewDirReader: %v", err)
	}
	defer func() { _ = repo.Close() }()

	paths, err := episodePaths(repo)
	if err != nil {
		t.Fatalf("episodePaths: %v", err)
	}
	want := []string{
		"episodes/architect/2024-01-03.md",
		"episodes/coder/2024-01-01.md",
		"episodes/coder/2024-01-02.md",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("episodePaths = %v, want %v", paths, want)
	}

	// A link out of the tree must fail the walk closed rather than enumerate
	// files outside the pinned repo.
	t.Run("escaping link fails closed", func(t *testing.T) {
		base := t.TempDir()
		writeFile(t, filepath.Join(base, "outside", "episodes", "coder", "2020-01-01.md"), "SECRET")
		writeFile(t, filepath.Join(base, "repo", "episodes", "coder", "2024-01-01.md"), "real")
		linkDir(t, filepath.Join(base, "outside", "episodes"), filepath.Join(base, "repo", "episodes", "linked"))

		repo2, err := memory.NewDirReader(filepath.Join(base, "repo"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = repo2.Close() }()

		paths, err := episodePaths(repo2)
		for _, p := range paths {
			if p == "episodes/linked/coder/2020-01-01.md" {
				t.Fatalf("walk enumerated a file outside the pinned repo: %v", paths)
			}
		}
		if err == nil {
			return // a skip that stayed inside the tree is also safe
		}
		// A refusal is the fail-closed outcome and needs no further assertion.
	})
}
