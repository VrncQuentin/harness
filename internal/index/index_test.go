package index

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		rootfs.WriteHooks{Sync: func(*os.File) error { return sentinel }})
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
		rootfs.WriteHooks{Sync: func(*os.File) error { return sentinel }})
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
	if _, err := CreateRooted(r, dir, 2); err != nil {
		t.Fatal(err)
	}
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	idx, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertRooted(r2, "a", "h1", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	// Reopen so the Root sees the published vectors.
	r3, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r3.Close() }()

	// Misaligned offset: vectors.bin is 8 bytes (one 2-dim float32).
	// Offset 1 is inside a float and should be rejected.
	bad := Manifest{Dim: 2, Count: 1}
	bad.Chunks = []Entry{{SHA: "a", Offset: 1, Length: 1}}
	err = validateManifestRooted(r3, dir, bad)
	if err == nil || !strings.Contains(err.Error(), "not vector-aligned") {
		t.Errorf("expected not-vector-aligned error, got %v", err)
	}

	// Truncate vectors to make file size unaligned.
	if err := r3.WriteStreamAtomic("vectors.bin", strings.NewReader("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reset manifest to match the truncated data.
	good := Manifest{Dim: 2, Count: 0}
	err = validateManifestRooted(r3, dir, good)
	if err == nil || !strings.Contains(err.Error(), "is not aligned") {
		t.Errorf("expected unaligned-file-size error, got %v", err)
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

// TestIndex_WriteManifestDoesNotRemoveStranger verifies finding 5.4:
// a post-rename os.Remove + retry fallback would delete a stranger's
// replacement that took over the destination name in the failure window.
// Manifest publication must leave that replacement untouched.
func TestIndex_WriteManifestDoesNotRemoveStranger(t *testing.T) {
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

	manifestPath := filepath.Join(dir, manifestFile)
	stranger := []byte("a stranger's replacement")
	renameFailed := errors.New("rename failed")

	// The publication is driven through the real WriteStreamAtomic lifecycle.
	// BeforeRename stages the exact state a fallback removal would face: the
	// rename is about to fail and a stranger already holds the destination
	// name.  The publication must abort without removing that stranger.
	err = idx.upsertRooted(r, "second", "second", [][]float32{{0, 1}}, rootfs.WriteHooks{
		BeforeRename: func() error {
			if rmErr := os.Rename(manifestPath, manifestPath+".aside"); rmErr != nil {
				return rmErr
			}
			if wErr := os.WriteFile(manifestPath, stranger, 0o644); wErr != nil {
				return wErr
			}
			return renameFailed
		},
	})
	if !errors.Is(err, renameFailed) {
		t.Fatalf("expected rename failure, got %v", err)
	}

	got, rErr := os.ReadFile(manifestPath)
	if rErr != nil {
		t.Fatalf("stranger replacement was removed: %v", rErr)
	}
	if !bytes.Equal(got, stranger) {
		t.Errorf("stranger replacement was modified: got %q, want %q", got, stranger)
	}
	// The real manifest was moved aside and must not have been deleted.
	if _, err := os.Stat(manifestPath + ".aside"); err != nil {
		t.Errorf("real manifest was deleted: %v", err)
	}
}

// TestIndex_WriteManifestFsyncsBeforeRename verifies finding 5.6:
// manifest publication must fsync before the rename, so a crash never
// leaves a half-written manifest at the destination path.  The failure is
// injected at the real Sync operation inside WriteStreamAtomic; if the
// publication renamed without waiting for that sync, the new entry would
// appear at manifest.json.
func TestIndex_WriteManifestFsyncsBeforeRename(t *testing.T) {
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

	oldManifest, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}

	syncFailed := errors.New("sync before rename failed")
	err = idx.upsertRooted(r, "new", "new", [][]float32{{0, 1}},
		rootfs.WriteHooks{Sync: func(*os.File) error { return syncFailed }})
	if !errors.Is(err, syncFailed) {
		t.Fatalf("expected sync-failed error, got %v", err)
	}

	// The destination must not have been renamed into place.
	newManifest, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(oldManifest) != string(newManifest) {
		t.Error("manifest.json was published despite a failed fsync — publication must wait for Sync")
	}

	// In-memory manifest must not include the new entry.
	if idx.Contains("new") {
		t.Error("new entry leaked into in-memory manifest after failed write")
	}
	if !idx.Contains("old") {
		t.Error("old entry was lost from in-memory manifest after failed write")
	}
}

// TestIndex_WriteManifestCleansUpOwnTemp verifies finding 5.7:
// temp-file cleanup deletes by name after a rename may have consumed the
// temp entry.  The real lifecycle must never remove an entry it did not
// create: a failed write leaves its own partial temp behind, and a stranger
// holding a temp-style name survives a successful publication.
func TestIndex_WriteManifestCleansUpOwnTemp(t *testing.T) {
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

	// A stranger holds a temp-style name.  A cleanup that removed temp-named
	// entries would delete it; the real lifecycle must not.
	strangerPath := filepath.Join(dir, ".harness-write-stranger")
	if err := os.WriteFile(strangerPath, []byte("stranger"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Successful publication runs the full temp lifecycle.
	if err := idx.UpsertRooted(r, "second", "second", [][]float32{{0, 1}}); err != nil {
		t.Fatal(err)
	}
	strangerData, err := os.ReadFile(strangerPath)
	if err != nil {
		t.Fatalf("stranger temp entry was cleaned up: %v", err)
	}
	if string(strangerData) != "stranger" {
		t.Errorf("stranger temp entry was modified: got %q", strangerData)
	}

	// A failed publication must propagate its error, leave its own partial
	// temp entry behind (never deleting it), and not reach manifest.json.
	syncFailed := errors.New("sync failed")
	err = idx.upsertRooted(r, "third", "third", [][]float32{{1, 1}},
		rootfs.WriteHooks{Sync: func(*os.File) error { return syncFailed }})
	if !errors.Is(err, syncFailed) {
		t.Fatalf("expected sync failure, got %v", err)
	}

	manifestData, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestData), "third") {
		t.Error("manifest.json was published despite the failed write")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ownTemp := false
	for _, e := range entries {
		switch e.Name() {
		case ".harness-write-stranger":
			// The stranger must still be here after the failed write too.
			data, readErr := os.ReadFile(strangerPath)
			if readErr != nil {
				t.Errorf("stranger temp entry disappeared after failed write: %v", readErr)
			} else if string(data) != "stranger" {
				t.Errorf("stranger temp entry modified after failed write: %q", data)
			}
		default:
			if strings.HasPrefix(e.Name(), ".harness-write-") {
				ownTemp = true
			}
		}
	}
	if !ownTemp {
		t.Error("own partial temp entry should survive a failed write")
	}
}

// TestIndex_TwoHandlesShareCoordinator verifies findings 5.8 and 5.9:
// mutations on one physical index directory must be serialized by a single
// coordinator shared across handles, and each write must start from the
// committed on-disk state.  Two handles writing concurrently must both be
// reflected in the published index — no entry may be lost to a stale
// in-memory manifest or to two unlocked writers overwriting each other.
func TestIndex_TwoHandlesShareCoordinator(t *testing.T) {
	dir := t.TempDir()
	r1, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r1.Close() }()
	r2, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()

	idx1, err := CreateRooted(r1, dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	// A second handle, opened before any entry is written, holds a stale
	// in-memory manifest.  Its write must adopt the other handle's committed
	// state instead of publishing over it.
	idx2, err := OpenRooted(r2, dir)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 2
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			handle, root := idx1, r1
			if n%2 == 1 {
				handle, root = idx2, r2
			}
			src := fmt.Sprintf("src-%d", n)
			if err := handle.UpsertRooted(root, src, src, [][]float32{{float32(n), float32(1 - n)}}); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	r3, err := rootfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r3.Close() }()
	idx3, err := OpenRooted(r3, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !idx3.Contains("src-0") || !idx3.Contains("src-1") {
		t.Fatalf("concurrent writes lost an entry: src-0=%v src-1=%v", idx3.Contains("src-0"), idx3.Contains("src-1"))
	}
	results, err := idx3.SearchRooted(r3, []float32{1, 0}, workers)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != workers {
		t.Fatalf("index has %d entries, want %d — a write was rolled back", len(results), workers)
	}
}
