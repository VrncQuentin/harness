package memoryops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/VrncQuentin/harness/internal/index"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/rootfs"
	"github.com/VrncQuentin/harness/internal/session"
)

func newTestEpisodeIndex(t *testing.T, projectRoot string) *EpisodeIndex {
	t.Helper()
	dr, err := memory.NewDirReader(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	indexDir := EpisodeIndexDir(projectRoot)
	if err := dr.MkdirAll("index/_episodes"); err != nil {
		t.Fatal(err)
	}
	a, err := dr.SubAnchor("index/_episodes")
	if err != nil {
		t.Fatal(err)
	}
	ei, err := NewEpisodeIndex(a, indexDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ei.Close() })
	return ei
}

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
	ei := newTestEpisodeIndex(t, root)
	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		IndexDir: indexDir,
		EI:       ei,
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
	r, err := rootfs.Open(indexDir)
	if err != nil {
		t.Fatalf("Open index dir: %v", err)
	}
	opened, err := index.OpenRooted(r, indexDir)
	_ = r.Close()
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

	dr, err := memory.NewDirReader(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dr.Close() })
	ei := newTestEpisodeIndex(t, root)

	// Corrupt the manifest after the EpisodeIndex is created.
	manifestPath := filepath.Join(indexDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"dim":2,`), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt manifest: %v", err)
	}

	rb := &EpisodeRebuilder{
		Mem:      dr,
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		IndexDir: indexDir,
		EI:       ei,
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
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll index dir: %v", err)
	}
	r, err := rootfs.Open(indexDir)
	if err != nil {
		t.Fatalf("Open index dir: %v", err)
	}
	idx, err := index.CreateRooted(r, indexDir, 2)
	_ = r.Close()
	if err != nil {
		t.Fatalf("Create index: %v", err)
	}
	r2, err := rootfs.Open(indexDir)
	if err != nil {
		t.Fatalf("Open index dir to seed: %v", err)
	}
	if err := idx.UpsertRooted(r2, "episodes/coder/ep1", contentHash("episode body"), [][]float32{{1, 0}}); err != nil {
		_ = r2.Close()
		t.Fatalf("seed index: %v", err)
	}
	_ = r2.Close()

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
		EI:       newTestEpisodeIndex(t, root),
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

	idxService := newTestEpisodeIndex(t, root)
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
		EI:       idxService,
	}
	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("rebuild re-embedded an already-indexed episode: embed calls = %d", emb.calls)
	}
}

func TestEpisodeIndexSharesNewlyCreatedHandleWithRetrieval(t *testing.T) {
	root := t.TempDir()
	service := newTestEpisodeIndex(t, root)
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

	// Place a stable alias at the real index path pointing outside the repo.
	outside := t.TempDir()
	if err := os.RemoveAll(indexDir); err != nil {
		t.Fatal(err)
	}

	// Try symlink first. SubAnchor navigates through OpenChild which
	// rejects links, so the Anchor construction itself fails.
	if err := os.Symlink(outside, indexDir); err == nil {
		dr, drErr := memory.NewDirReader(repo)
		if drErr != nil {
			t.Fatal(drErr)
		}
		defer func() { _ = dr.Close() }()
		_, err := dr.SubAnchor("index/_episodes")
		_ = os.Remove(indexDir)
		if err == nil {
			t.Fatal("SubAnchor accepted symlink at index/_episodes")
		}
		t.Logf("SubAnchor rejected symlink: %v", err)
		return
	}

	// Try Windows junction. SubAnchor traverses through OpenChild which
	// rejects links.
	cmd := exec.Command("cmd", "/c", "mklink", "/J", indexDir, outside)
	if out, err := cmd.CombinedOutput(); err == nil {
		dr, drErr := memory.NewDirReader(repo)
		if drErr != nil {
			t.Fatal(drErr)
		}
		defer func() { _ = dr.Close() }()
		_, err := dr.SubAnchor("index/_episodes")
		_ = os.RemoveAll(indexDir)
		if err == nil {
			t.Fatal("SubAnchor accepted junction at index/_episodes")
		}
		t.Logf("SubAnchor rejected junction: %v", err)
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

	dr, err := memory.NewDirReader(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if err := dr.MkdirAll("index/_episodes"); err != nil {
		t.Fatal(err)
	}
	a, err := dr.SubAnchor("index/_episodes")
	if err != nil {
		t.Fatal(err)
	}
	ei, err := NewEpisodeIndex(a, indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ei.Close() }()

	// Create the index so we can test both missing and existing branches.
	err = ei.Upsert("ep1", "abc", [][]float32{{1, 0}})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Try to remove the directory while the Anchor holds it.
	if err := os.RemoveAll(indexDir); err != nil {
		_ = ei.Close()
		if err := os.RemoveAll(indexDir); err != nil {
			t.Fatal("removal should succeed after Anchor closed:", err)
		}
		return
	}

	// Non-Windows: removal succeeded despite open handle. Replace
	// the directory and verify operations fail closed.
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err = ei.Upsert("ep2", "def", [][]float32{{0, 1}})
	if err == nil {
		t.Fatal("Upsert should fail after directory replacement")
	}

	_, err = ei.Search([]float32{1, 0}, 1)
	if err == nil {
		t.Fatal("Search should fail after directory replacement")
	}
}
