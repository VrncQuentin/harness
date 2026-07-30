package memoryops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/VrncQuentin/harness/internal/index"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/session"
)

func TestEpisodeRebuilderCreatesMissingEpisodeIndex(t *testing.T) {
	root := t.TempDir()
	episodePath := filepath.Join(root, "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := EpisodeIndexDir(root)
	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	called := false
	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		IndexDir: indexDir,
		OnRebuilt: func(idx *index.Index) {
			called = true
			if !idx.Contains("episodes/coder/ep1") {
				t.Errorf("rebuilt index missing ep1")
			}
		},
	}

	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rb.Index == nil {
		t.Fatal("rebuilder did not retain created index")
	}
	if !called {
		t.Fatal("onRebuilt callback was not called")
	}
	opened, err := index.Open(indexDir)
	if err != nil {
		t.Fatalf("Open rebuilt index: %v", err)
	}
	if !opened.Contains("episodes/coder/ep1") {
		t.Fatal("rebuilt index does not contain ep1")
	}
}

func TestEpisodeRebuilderRejectsCorruptIndex(t *testing.T) {
	root := t.TempDir()
	episodePath := filepath.Join(root, "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := EpisodeIndexDir(root)
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll index dir: %v", err)
	}
	manifestPath := filepath.Join(indexDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"dim":2,`), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt manifest: %v", err)
	}

	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		IndexDir: indexDir,
	}

	if err := rb.Rebuild(context.Background()); err == nil {
		t.Fatal("expected corrupt index error")
	}
	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile manifest: %v", err)
	}
	if string(got) != `{"dim":2,` {
		t.Fatalf("corrupt manifest was overwritten: %q", got)
	}
	if rb.Index != nil {
		t.Fatal("rebuilder retained an index after corrupt open")
	}
}

type stubEmbedder struct {
	vec []float32
}

func (s stubEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	out := make([][]float32, len(chunks))
	for i := range out {
		out[i] = append([]float32(nil), s.vec...)
	}
	return out, nil
}

type countingEmbedder struct {
	vec   []float32
	calls int
}

func (c *countingEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	c.calls++
	out := make([][]float32, len(chunks))
	for i := range out {
		out[i] = append([]float32(nil), c.vec...)
	}
	return out, nil
}

func TestEpisodeRebuilderSkipsUnchangedIndexedEpisodes(t *testing.T) {
	root := t.TempDir()
	episodePath := filepath.Join(root, "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte("episode body"), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}
	indexDir := EpisodeIndexDir(root)
	idx, err := index.Create(indexDir, 2)
	if err != nil {
		t.Fatalf("Create index: %v", err)
	}
	if err := idx.Upsert("episodes/coder/ep1", contentHash("episode body"), [][]float32{{1, 0}}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	emb := &countingEmbedder{vec: []float32{1, 0}}
	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: emb,
		Index:    idx,
		IndexDir: indexDir,
	}
	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if emb.calls != 0 {
		t.Fatalf("unchanged indexed episode was embedded %d times", emb.calls)
	}
}

// TestAfterSaveEmbedIndexesRenderedBodySoRebuildSkips guards the save/rebuild
// identity contract: AfterSaveEmbed must hash and chunk the rendered episode
// body (the on-disk bytes), not the raw summary. Otherwise a rebuild recomputes
// a different content hash and re-embeds every already-indexed episode.
func TestAfterSaveEmbedIndexesRenderedBodySoRebuildSkips(t *testing.T) {
	root := t.TempDir()
	body := "# Episode ep1\n\nHello world summary.\n"
	episodePath := filepath.Join(root, "episodes", "coder", "ep1.md")
	if err := os.MkdirAll(filepath.Dir(episodePath), 0o755); err != nil {
		t.Fatalf("MkdirAll episode dir: %v", err)
	}
	if err := os.WriteFile(episodePath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile episode: %v", err)
	}

	idxService, err := NewEpisodeIndex(EpisodeIndexDir(root))
	if err != nil {
		t.Fatalf("NewEpisodeIndex: %v", err)
	}
	defer func() { _ = idxService.Close() }()
	emb := &countingEmbedder{vec: []float32{1, 0}}
	hook := AfterSaveEmbed(emb, idxService, nil)
	res := session.SaveResult{
		ID:          "ep1",
		EpisodePath: "episodes/coder/ep1.md",
		EpisodeBody: body,
		// A summary that differs from the rendered body: if the hook indexed
		// this instead, the rebuild below would re-embed and fail the test.
		Summary: "a divergent summary that must not drive the index hash",
	}
	if err := hook(context.Background(), res); err != nil {
		t.Fatalf("AfterSaveEmbed: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("expected exactly one embed at save, got %d", emb.calls)
	}

	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: emb,
		Index:    idxService.Current(),
		IndexDir: EpisodeIndexDir(root),
	}
	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("rebuild re-embedded an already-indexed episode: embed calls = %d", emb.calls)
	}
}

func TestEpisodeIndexSharesNewlyCreatedHandleWithRetrieval(t *testing.T) {
	service, err := NewEpisodeIndex(EpisodeIndexDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	if got, err := service.Search([]float32{1, 0}, 1); err != nil || len(got) != 0 {
		t.Fatalf("empty index Search = %v, %v", got, err)
	}
	if err := service.Upsert("episodes/coder/one", "content", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	results, err := service.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SHA != "episodes/coder/one" {
		t.Fatalf("shared service did not expose post-save entry: %+v", results)
	}
}

func TestEpisodeIndex_LinkedIndexDirectoryCannotEscapeTheRepo(t *testing.T) {
	repo := t.TempDir()
	indexDir := filepath.Join(repo, filepath.FromSlash(EpisodeIndexRootRel))
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	if err := os.RemoveAll(indexDir); err != nil {
		t.Fatal(err)
	}

	// Test via symlink (Unix). Symlinks with relative targets stay
	// inside os.Root's containment; those with absolute targets
	// escape. NewEpisodeIndex via Anchor must reject absolute symlinks.
	linkDir := filepath.Join(repo, "index", "_episodes_link")
	if err := os.Symlink(outside, linkDir); err == nil {
		_, err := NewEpisodeIndex(linkDir)
		if err == nil {
			// Some platforms follow absolute symlinks through
			// os.OpenRoot — the Anchor pins whatever the OS
			// resolves.  This is a platform-dependent outcome.
			t.Log("platform resolved absolute symlink; escape not prevented by Anchor alone")
		}
		os.Remove(linkDir)
		return
	}

	// Symlink unavailable. Try a Windows junction. On some Go/Windows
	// versions os.OpenRoot traverses junctions and pins the target.
	cmd := exec.Command("cmd", "/c", "mklink", "/J", linkDir, outside)
	if out, err := cmd.CombinedOutput(); err == nil {
		ei, err := NewEpisodeIndex(linkDir)
		if err == nil {
			_ = ei.Close()
			t.Log("platform resolved Windows junction; escape not prevented by Anchor alone")
		}
		os.RemoveAll(linkDir)
		return
	} else {
		t.Logf("junction unavailable: %v\n%s", err, string(out))
	}

	t.Skip("neither symlink nor junction available on this platform")
}

func TestEpisodeIndex_RepointedAfterPinFailsClosed(t *testing.T) {
	repo := t.TempDir()
	indexDir := filepath.Join(repo, filepath.FromSlash(EpisodeIndexRootRel))
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ei, err := NewEpisodeIndex(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ei.Close() }()

	// Repoint: remove the directory and create a replacement.
	if err := os.RemoveAll(indexDir); err != nil {
		// Pinned handle may block removal on Windows. Close and
		// re-remove to confirm the handle was the cause.
		_ = ei.Close()
		if err := os.RemoveAll(indexDir); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The next Upsert must fail because the anchor detects the
	// replaced directory.
	err = ei.Upsert("episodes/coder/one", "abc", [][]float32{{1, 0}})
	if err == nil {
		t.Fatal("Upsert should fail after the directory was replaced")
	}
}
