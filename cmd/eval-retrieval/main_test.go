package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
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

// stubEmbedder returns a fixed vector so the production scoring path can run
// without a sidecar. The episode index is empty in these tests, so semantic
// retrieval contributes nothing; the discrimination is about enumeration.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	vecs := make([][]float32, len(chunks))
	for i := range vecs {
		vecs[i] = []float32{0.1, 0.2}
	}
	return vecs, nil
}

// Finding 3.6: eval-retrieval must enumerate episodes through a pinned repo
// reader, producing stable repo-relative forward-slash paths, rather than
// filepath.Glob + filepath.Rel on an operator-supplied root. The test drives
// evaluate — the exact function run() executes — and asserts the paths it
// returns, so reverting enumeration to pathname globs changes the result.
func TestEvalRetrieval_PinnedRepo(t *testing.T) {
	t.Run("pinned enumeration", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
		writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-02.md"), "two")
		writeFile(t, filepath.Join(root, "episodes", "architect", "2024-01-03.md"), "three")
		// .md files outside the depth-two episode shape must not be enumerated.
		writeFile(t, filepath.Join(root, "rules.md"), "not an episode")
		writeFile(t, filepath.Join(root, "episodes", "top.md"), "not an episode")

		queries := []queryRecord{{Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}}
		paths, err := evaluate(root, queries, 5, stubEmbedder{})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		want := []string{
			"episodes/architect/2024-01-03.md",
			"episodes/coder/2024-01-01.md",
			"episodes/coder/2024-01-02.md",
		}
		if !slices.Equal(paths, want) {
			t.Errorf("evaluated paths = %v, want %v", paths, want)
		}
	})

	// The escape the old mechanism would follow: a directory link at
	// episodes/linked pointing at a tree with a directly-matching .md. The old
	// episodes/*/*.md glob would list <repo>/episodes/linked/*.md and match the
	// outside file through the link; the pinned walk never follows the link, so
	// the outside path is not among the enumerated paths. Whatever the walk
	// outcome (a refusal is also safe), the outside path must never appear.
	t.Run("escaping link excluded", func(t *testing.T) {
		base := t.TempDir()
		writeFile(t, filepath.Join(base, "outside", "episodes", "leak.md"), "SECRET")
		writeFile(t, filepath.Join(base, "repo", "episodes", "coder", "2024-01-01.md"), "real")
		linkDir(t, filepath.Join(base, "outside", "episodes"), filepath.Join(base, "repo", "episodes", "linked"))

		queries := []queryRecord{{Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}}
		paths, err := evaluate(filepath.Join(base, "repo"), queries, 5, stubEmbedder{})
		for _, p := range paths {
			if p == "episodes/linked/leak.md" {
				t.Fatalf("outside file was enumerated through the link: %v", paths)
			}
		}
		if err != nil {
			return // a refusal is the fail-closed outcome
		}
		if !slices.Contains(paths, "episodes/coder/2024-01-01.md") {
			t.Errorf("real episode missing from enumeration: %v", paths)
		}
	})
}
