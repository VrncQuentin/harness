package memoryops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/session"
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
	called := false
	rb := &EpisodeRebuilder{
		Mem:      memory.NewDirReader(root),
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

	rb := &EpisodeRebuilder{
		Mem:      memory.NewDirReader(root),
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
	rb := &EpisodeRebuilder{
		Mem:      memory.NewDirReader(root),
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
		Mem:      memory.NewDirReader(root),
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
