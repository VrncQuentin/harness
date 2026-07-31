package index

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

func TestIndex_CreatePersistsEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := CreateRooted(r, dir, 2); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenRooted(r, dir)
	if err != nil {
		t.Fatalf("Open newly created index: %v", err)
	}
	results, err := opened.SearchRooted(r, []float32{1, 0}, 5)
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
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, err = OpenRooted(r, dir)
	if err == nil {
		t.Fatal("expected missing vectors to be rejected")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing vectors were reported as a missing index: %v", err)
	}
}

func TestIndex_OpenRejectsVectorBoundsMismatch(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "sha", "sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, vectorsFile), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRooted(r, dir); err == nil {
		t.Fatal("expected manifest entry extending past vectors file to be rejected")
	}
}

func TestIndex_UpsertManifestFailurePreservesOldIndex(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "old", "old", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	vecSizeBefore := fileSize(t, filepath.Join(dir, vectorsFile))

	sentinel := errors.New("injected manifest failure")
	err = idx.upsertRooted(r, "new", "new-content", [][]float32{{0, 1}},
		func(root *rootfs.Root, data []byte) error { return sentinel })
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if !idx.Contains("old") {
		t.Fatal("old entry should still be in manifest")
	}
	if idx.Contains("new") {
		t.Fatal("failed upsert should not be in memory")
	}
	vecSizeAfter := fileSize(t, filepath.Join(dir, vectorsFile))
	if vecSizeAfter <= vecSizeBefore {
		t.Error("vectors.bin should have grown")
	}
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx2, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}
	results, err := idx2.SearchRooted(r2, []float32{1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SHA != "old" {
		t.Fatalf("old entry should be searchable: %v", results)
	}
	if idx2.Contains("new") {
		t.Fatal("new entry should not be present after recovery")
	}
}

func TestIndex_UpsertReplacesViaRename(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "first", "first", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	vectorsPath := filepath.Join(dir, vectorsFile)
	// Create a hard link to the vectors file as a sentinel.
	sentinel := filepath.Join(dir, "sentinel.bin")
	if err := os.Link(vectorsPath, sentinel); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "second", "second", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	// The sentinel must still contain only the original data.
	sentData, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if len(sentData) != 8 { // one vector of 2 floats = 8 bytes
		t.Fatalf("sentinel should be unchanged, got %d bytes", len(sentData))
	}
	// The actual vectors file must have grown (two vectors = 16 bytes).
	vData, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(vData) != 16 {
		t.Fatalf("vectors.bin should have two vectors (16 bytes), got %d", len(vData))
	}
}

func TestIndex_UpsertReplacesMiddleEntry(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "a", "a", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "b", "b", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "c", "c", [][]float32{{1, 1}}); err != nil {
		t.Fatal(err)
	}
	// Replace the middle entry.
	if err := idx.UpsertRooted(r, "b", "new-b", [][]float32{{2, 2}}); err != nil {
		t.Fatal(err)
	}
	if !idx.ContainsCurrent("b", "new-b") {
		t.Error("b should be present with new content hash")
	}
	if !idx.Contains("a") {
		t.Error("a should still be present")
	}
	if !idx.Contains("c") {
		t.Error("c should still be present")
	}
}

func TestIndex_UpsertFailurePreservesOtherEntries(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "a", "a", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "b", "b", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected manifest failure")
	err = idx.upsertRooted(r, "b", "new-b", [][]float32{{2, 2}},
		func(root *rootfs.Root, data []byte) error { return sentinel })
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if !idx.Contains("b") {
		t.Fatal("b should still be in manifest after failed upsert")
	}
	if idx.ContainsCurrent("b", "new-b") {
		t.Fatal("b should not have new content hash")
	}
	if !idx.Contains("a") {
		t.Fatal("a should still be present")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func TestIndex_AddSearchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 4)
	if err != nil {
		t.Fatal(err)
	}

	// Add two SHAs with embeddings.
	v1 := [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}}
	if err := idx.UpsertRooted(r, "sha-aaa", "sha-aaa", v1); err != nil {
		t.Fatal(err)
	}
	v2 := [][]float32{{0, 0, 1, 0}}
	if err := idx.UpsertRooted(r, "sha-bbb", "sha-bbb", v2); err != nil {
		t.Fatal(err)
	}

	// Add duplicate SHA is a no-op.
	if err := idx.UpsertRooted(r, "sha-aaa", "sha-aaa", [][]float32{{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// Search for a vector close to the first chunk.
	query := []float32{1, 0, 0, 0}
	results, err := idx.SearchRooted(r, query, 2)
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
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	err = idx.UpsertRooted(r, "sha", "sha", [][]float32{{1, 2, 3, 4}})
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestIndex_AddVectorFailureDoesNotPoisonManifest(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
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
	if err := idx.UpsertRooted(r, "bad", "bad", [][]float32{{1, 0}}); err == nil {
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
	if err := idx.UpsertRooted(r, "good", "good", [][]float32{{0, 1}}); err != nil {
		t.Fatalf("second Add: %v", err)
	}

	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	opened, err := OpenRooted(r2, dir)
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
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	_, err = idx.SearchRooted(r, []float32{1, 2}, 5)
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestIndex_OpenAndSearch(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "sha", "sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}

	// Re-open and verify.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx2, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}
	results, err := idx2.SearchRooted(r2, []float32{1, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SHA != "sha" {
		t.Errorf("re-opened index: got %+v", results)
	}
}

func TestIndex_OpenMissing(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, err = OpenRooted(r, dir)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected ErrNotExist for missing manifest, got %v", err)
	}
}

func TestIndex_Contains(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "sha-abc", "sha-abc", [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if !idx.Contains("sha-abc") {
		t.Error("expected sha-abc to be found")
	}
	if idx.Contains("sha-xyz") {
		t.Error("expected sha-xyz to not be found")
	}
	// Re-open and check persistence.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx2, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.Contains("sha-abc") {
		t.Error("re-opened: expected sha-abc to be found")
	}
}

func TestIndex_ContainsCurrentMatchesSourceAndContentHash(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "episodes/coder/ep1", "hash-a", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if !idx.ContainsCurrent("episodes/coder/ep1", "hash-a") {
		t.Fatal("expected source with matching hash to be current")
	}
	if idx.ContainsCurrent("episodes/coder/ep1", "hash-b") {
		t.Fatal("changed content hash should not be current")
	}
	if idx.ContainsCurrent("episodes/reviewer/ep1", "hash-a") {
		t.Fatal("same hash under another source should not be current")
	}
	if idx.ContainsCurrent("episodes/coder/ep1", "") {
		t.Fatal("empty hash should not be current")
	}
}

func TestIndex_UpsertReplacesSourceAndKeepsAgentPathsDistinct(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	coder := "episodes/coder/shared"
	reviewer := "episodes/reviewer/shared"
	if err := idx.UpsertRooted(r, coder, "first", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, reviewer, "first", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, coder, "second", [][]float32{{-1, 0}}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.SearchRooted(r, []float32{1, 0}, 2)
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

func TestEntryRange_Overflow(t *testing.T) {
	tests := []struct {
		name    string
		offset  int64
		length  int
		dim     int
		size    int64
		wantErr string
	}{
		{"negative length", 0, -1, 2, 100, "negative length"},
		{"zero dim", 0, 1, 0, 100, "non-positive dim"},
		{"negative dim", 0, 1, -1, 100, "non-positive dim"},
		{"negative offset", -1, 1, 2, 100, "negative offset"},
		{"end overflow", math.MaxInt64, 1, 2, math.MaxInt64, "end overflow"},
		{"exceeds file size", 0, 100, 4, 8, "exceeds file size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := entryRange(tc.offset, tc.length, tc.dim, tc.size)
			if err == nil {
				t.Error("expected error, got nil")
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateManifestRooted_Alignment(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	// Create a valid manifest + vectors pair.
	if _, err := CreateRooted(r, dir, 2); err != nil {
		t.Fatal(err)
	}

	// Reopen to get the manifest we just wrote.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the manifest with a misaligned offset.
	bad := Manifest{Dim: idx.manifest.Dim, Count: idx.manifest.Count}
	bad.Chunks = []Entry{{SHA: "x", Offset: 1, Length: 1}}
	err = validateManifestRooted(r2, dir, bad)
	if err == nil {
		t.Error("expected error for misaligned offset")
	}

	// Truncate vectors to make file size unaligned.
	if err := r2.WriteStreamAtomic("vectors.bin", strings.NewReader("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = validateManifestRooted(r2, dir, idx.manifest)
	if err == nil {
		t.Error("expected error for unaligned file size")
	}
}

func TestEntryRange_ValidReturnsEnd(t *testing.T) {
	end, err := entryRange(0, 2, 3, 2*3*4)
	if err != nil {
		t.Fatal(err)
	}
	if end != 24 {
		t.Fatalf("end = %d, want 24", end)
	}
}

func TestSearchRooted_TruncatedVectorsReturnsError(t *testing.T) {
	dir := t.TempDir()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	idx, err := CreateRooted(r, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "a", "h1", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r, "b", "h2", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}

	// Reopen to get the updated manifest.
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx2, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}

	// Truncate vectors.bin to corrupt offset ranges.
	if err := r2.WriteStreamAtomic("vectors.bin", strings.NewReader("short"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = idx2.SearchRooted(r2, []float32{1, 0}, 5)
	if err == nil {
		t.Fatal("expected error for truncated vectors, got nil")
	}
}
