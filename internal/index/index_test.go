package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestIndex_CreatePersistsEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir, 2); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open newly created index: %v", err)
	}
	results, err := opened.Search([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("Search empty index: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("empty index results = %+v", results)
	}
}

func TestIndex_OpenRejectsManifestWithoutVectors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(`{"dim":2,"count":1,"chunks":[{"sha":"sha","offset":0,"length":1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected missing vectors to be rejected")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing vectors were reported as a missing index: %v", err)
	}
}

func TestIndex_OpenRejectsVectorBoundsMismatch(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, vectorsFile), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("expected manifest entry extending past vectors file to be rejected")
	}
}

func TestIndex_UpsertManifestFailureRollsBackVectors(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("old", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	vectorsPath := filepath.Join(dir, vectorsFile)
	before, err := os.Stat(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, manifestFile)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestPath, "block"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert("new", "new-content", [][]float32{{0, 1}}); err == nil {
		t.Fatal("expected manifest write to fail")
	}
	after, err := os.Stat(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("vectors size after failed manifest write = %d, want %d", after.Size(), before.Size())
	}
	if idx.Contains("new") {
		t.Fatal("failed upsert remained in memory")
	}
}

func TestIndex_AddSearchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 4)
	if err != nil {
		t.Fatal(err)
	}

	// Add two SHAs with embeddings.
	v1 := [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}}
	if err := idx.Add("sha-aaa", v1); err != nil {
		t.Fatal(err)
	}
	v2 := [][]float32{{0, 0, 1, 0}}
	if err := idx.Add("sha-bbb", v2); err != nil {
		t.Fatal(err)
	}

	// Add duplicate SHA is a no-op.
	if err := idx.Add("sha-aaa", [][]float32{{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// Search for a vector close to the first chunk.
	query := []float32{1, 0, 0, 0}
	results, err := idx.Search(query, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].SHA != "sha-aaa" {
		t.Errorf("top result: got %s, want sha-aaa", results[0].SHA)
	}
	if results[0].Score < 0.99 {
		t.Errorf("score for identical vector: got %v, want ~1.0", results[0].Score)
	}
}

func TestIndex_AddDimensionMismatch(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	err = idx.Add("sha", [][]float32{{1, 2, 3, 4}})
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestIndex_AddVectorFailureDoesNotPoisonManifest(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	vectorsPath := filepath.Join(dir, vectorsFile)
	if err := os.Remove(vectorsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(vectorsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("bad", [][]float32{{1, 0}}); err == nil {
		t.Fatal("expected vector append to fail when vectors path is a directory")
	}
	if idx.Contains("bad") {
		t.Fatal("failed add should not remain in in-memory manifest")
	}
	if err := os.Remove(vectorsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vectorsPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("good", [][]float32{{0, 1}}); err != nil {
		t.Fatalf("second Add: %v", err)
	}

	opened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Contains("bad") {
		t.Fatal("failed add was persisted by later successful Add")
	}
	if !opened.Contains("good") {
		t.Fatal("successful add missing from reopened manifest")
	}
}
func TestIndex_SearchDimensionMismatch(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	_, err = idx.Search([]float32{1, 2}, 5)
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestIndex_OpenAndSearch(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}

	// Re-open and verify.
	idx2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	results, err := idx2.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SHA != "sha" {
		t.Errorf("re-opened index: got %+v", results)
	}
}

func TestIndex_OpenMissing(t *testing.T) {
	_, err := Open("/nonexistent/index/path")
	if err == nil {
		t.Fatal("expected error for missing index")
	}
}

func TestIndex_Contains(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("sha-abc", [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("sha-abc") {
		t.Error("expected sha-abc to be found")
	}
	if idx.Contains("sha-xyz") {
		t.Error("expected sha-xyz to not be found")
	}
	// Re-open and check persistence.
	idx2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.Contains("sha-abc") {
		t.Error("re-opened: expected sha-abc to be found")
	}
}

func TestIndex_UpsertReplacesSourceAndKeepsAgentPathsDistinct(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	coder := "episodes/coder/shared"
	reviewer := "episodes/reviewer/shared"
	if err := idx.Upsert(coder, "first", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(reviewer, "first", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(coder, "second", [][]float32{{-1, 0}}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search([]float32{1, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].SHA != reviewer {
		t.Fatalf("resaved source remained searchable: top result = %q, want %q", results[0].SHA, reviewer)
	}
	if !idx.Contains(coder) || !idx.Contains(reviewer) {
		t.Fatal("source-path identities were not retained")
	}
}
