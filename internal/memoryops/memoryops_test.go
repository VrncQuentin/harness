package memoryops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	mem := openTestRepo(t, root)
	idxService := openTestIndex(t, mem)
	rb := &EpisodeRebuilder{
		Mem:      mem,
		Embedder: stubEmbedder{vec: []float32{1, 0}},
		Index:    idxService,
	}

	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if !idxService.Ready() {
		t.Fatal("rebuilder did not create the index")
	}
	if !idxService.Contains("episodes/coder/ep1") {
		t.Fatal("rebuilt index missing ep1")
	}
	// Re-open from disk so the assertion is about what was persisted rather
	// than about the in-memory manifest.
	reopened := openTestIndex(t, mem)
	if !reopened.Contains("episodes/coder/ep1") {
		t.Fatal("rebuilt index does not contain ep1 on disk")
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
	indexDir := filepath.Join(root, filepath.FromSlash(EpisodeIndexRootRel))
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("MkdirAll index dir: %v", err)
	}
	manifestPath := filepath.Join(indexDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"dim":2,`), 0o644); err != nil {
		t.Fatalf("WriteFile corrupt manifest: %v", err)
	}

	mem := openTestRepo(t, root)
	if _, err := NewEpisodeIndex(mem, EpisodeIndexRootRel); err == nil {
		t.Fatal("expected corrupt index error")
	}
	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile manifest: %v", err)
	}
	if string(got) != `{"dim":2,` {
		t.Fatalf("corrupt manifest was overwritten: %q", got)
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
	mem := openTestRepo(t, root)
	idx := openTestIndex(t, mem)
	if err := idx.Upsert("episodes/coder/ep1", contentHash("episode body"), [][]float32{{1, 0}}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	emb := &countingEmbedder{vec: []float32{1, 0}}
	rb := &EpisodeRebuilder{
		Mem:      mem,
		Embedder: emb,
		Index:    idx,
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

	mem := openTestRepo(t, root)
	idxService := openTestIndex(t, mem)
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

	rb := &EpisodeRebuilder{
		Mem:      mem,
		Embedder: emb,
		Index:    idxService,
	}
	if err := rb.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if emb.calls != 1 {
		t.Fatalf("rebuild re-embedded an already-indexed episode: embed calls = %d", emb.calls)
	}
}

func TestEpisodeIndexSharesNewlyCreatedHandleWithRetrieval(t *testing.T) {
	service := openTestIndex(t, openTestRepo(t, t.TempDir()))
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

// openTestRepo pins a project memory repo for a test and closes it on cleanup.
func openTestRepo(t *testing.T, root string) *memory.DirReader {
	t.Helper()
	r, err := memory.OpenDirReader(root)
	if err != nil {
		t.Fatalf("OpenDirReader %s: %v", root, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// openTestIndex opens the episode index inside a pinned repo, the same way the
// runtime does, and closes it on cleanup.
func openTestIndex(t *testing.T, mem memory.Repo) *EpisodeIndex {
	t.Helper()
	idx, err := NewEpisodeIndex(mem, EpisodeIndexRootRel)
	if err != nil {
		t.Fatalf("NewEpisodeIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}
