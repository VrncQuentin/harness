package index

import (
	"math"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1.0},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0.0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1.0},
		{"zero a", []float32{0, 0}, []float32{1, 2}, 0.0},
		{"zero b", []float32{1, 2}, []float32{0, 0}, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(float64(got-tt.want)) > 0.001 {
				t.Errorf("cosineSimilarity() = %v, want %v", got, tt.want)
			}
		})
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
