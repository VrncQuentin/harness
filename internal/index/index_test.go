package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/VrncQuentin/harness/internal/rootfs"
)

// pin opens dir as a rooted capability for the duration of a test. Production
// callers get theirs by resolving the index's location through the project
// memory repo's handle; a test that only exercises the index itself needs the
// same shape without a repo around it.
func pin(t *testing.T, dir string) *rootfs.Root {
	t.Helper()
	r, err := rootfs.Open(dir)
	if err != nil {
		t.Fatalf("rootfs.Open %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestIndex_CreatePersistsEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(pin(t, dir), "index", 2); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(pin(t, dir), "index")
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
	_, err := Open(pin(t, dir), "index")
	if err == nil {
		t.Fatal("expected missing vectors to be rejected")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing vectors were reported as a missing index: %v", err)
	}
}

func TestIndex_OpenRejectsVectorBoundsMismatch(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(pin(t, dir), "index", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, vectorsFile), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(pin(t, dir), "index"); err == nil {
		t.Fatal("expected manifest entry extending past vectors file to be rejected")
	}
}

// A failed manifest write no longer rolls vectors.bin back by truncating it in
// place — that would reopen the vectors.bin file read-write and shorten it,
// which is exactly the operation that corrupts an outside file if the entry
// happens to be a hard link to one. Upsert now publishes the whole new
// vectors.bin (old content plus the addition) by rename before it ever touches
// the manifest, so a manifest failure simply leaves those new bytes
// unreferenced by any chunk: harmless, not a corruption, and nothing to roll
// back.
func TestIndex_UpsertManifestFailureLeavesVectorsPublishedButUnreferenced(t *testing.T) {
	dir := t.TempDir()
	root := pin(t, dir)
	idx, err := Create(root, "index", 2)
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
	if idx.Contains("new") {
		t.Fatal("failed upsert remained in the in-memory manifest")
	}

	after, err := os.Stat(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	const oneVector = 2 * 4 // dim 2, float32
	if want := before.Size() + oneVector; after.Size() != want {
		t.Fatalf("vectors size after failed manifest write = %d, want %d (published, not rolled back)", after.Size(), want)
	}

	// The extra bytes must not corrupt anything reopened afterward: the
	// manifest directory is still blocked, so Open should fail the same way
	// Create's caller would — but restoring a real manifest.json and
	// reopening must see exactly the pre-failure state, with the orphaned
	// bytes simply never referenced.
	if err := os.RemoveAll(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := idx.writeManifest(); err != nil {
		t.Fatalf("writeManifest after clearing the block: %v", err)
	}
	reopened, err := Open(root, "index")
	if err != nil {
		t.Fatalf("Open after recovering the manifest: %v", err)
	}
	if reopened.Contains("new") {
		t.Fatal("the orphaned vectors resurfaced as a manifest entry after reopening")
	}
	if !reopened.Contains("old") {
		t.Fatal("the original entry was lost")
	}
}

// A subsequent successful Upsert after a manifest-write failure must still
// work correctly: the orphaned bytes left behind by the failed attempt must
// not corrupt the offsets of vectors added afterward.
func TestIndex_UpsertSucceedsAfterAPriorManifestFailure(t *testing.T) {
	dir := t.TempDir()
	root := pin(t, dir)
	idx, err := Create(root, "index", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("old", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(dir, manifestFile)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert("failed", "content", [][]float32{{0, 1}}); err == nil {
		t.Fatal("expected manifest write to fail")
	}
	if err := os.RemoveAll(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := idx.writeManifest(); err != nil {
		t.Fatalf("writeManifest after clearing the block: %v", err)
	}

	if err := idx.Add("after", [][]float32{{1, 1}}); err != nil {
		t.Fatalf("Add after a prior manifest failure: %v", err)
	}
	results, err := idx.Search([]float32{1, 1}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].SHA != "after" {
		t.Fatalf("results = %+v, want [after]", results)
	}
}

func TestIndex_AddSearchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(pin(t, dir), "index", 4)
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
	idx, err := Create(pin(t, dir), "index", 3)
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
	idx, err := Create(pin(t, dir), "index", 2)
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

	opened, err := Open(pin(t, dir), "index")
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
	idx, err := Create(pin(t, dir), "index", 4)
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
	idx, err := Create(pin(t, dir), "index", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("sha", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}

	// Re-open and verify.
	idx2, err := Open(pin(t, dir), "index")
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
	_, err := Open(pin(t, t.TempDir()), "index")
	if err == nil {
		t.Fatal("expected error for missing index")
	}
}

func TestIndex_Contains(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(pin(t, dir), "index", 3)
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
	idx2, err := Open(pin(t, dir), "index")
	if err != nil {
		t.Fatal(err)
	}
	if !idx2.Contains("sha-abc") {
		t.Error("re-opened: expected sha-abc to be found")
	}
}

func TestIndex_ContainsCurrentMatchesSourceAndContentHash(t *testing.T) {
	dir := t.TempDir()
	idx, err := Create(pin(t, dir), "index", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert("episodes/coder/ep1", "hash-a", [][]float32{{1, 0}}); err != nil {
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
	idx, err := Create(pin(t, dir), "index", 2)
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

// vectors.bin is a cache the harness itself created, but nothing stops another
// name inside the index directory from being a hard link to it, or vectors.bin
// itself from being a hard-linked *entry* pointing at a file elsewhere on the
// same volume. An earlier version opened vectors.bin read-write and appended
// to it in place, which writes through exactly that link: growing the shared
// inode extends whatever else the link names too. Upsert now assembles the new
// vectors.bin in memory and publishes it by rename, which replaces the
// directory entry rather than writing through it, so a file hard-linked from
// outside the index directory is untouched by an Upsert that only ever meant
// to grow the index's own cache.
func TestIndex_UpsertDoesNotCorruptAHardLinkedVectorsFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "index")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root := pin(t, dir)
	idx, err := Create(root, "index", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("one", [][]float32{{1, 0}}); err != nil {
		t.Fatal(err)
	}

	// A second name for the same vectors.bin inode, sitting outside the index
	// directory entirely — standing in for "some other file on the same
	// volume that happens to share this inode by accident or attack".
	outsideLink := filepath.Join(base, "outside-bait.bin")
	if err := os.Link(filepath.Join(dir, vectorsFile), outsideLink); err != nil {
		t.Fatalf("hard links are expected to work here: %v", err)
	}
	before, err := os.ReadFile(outsideLink)
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.Upsert("two", "second", [][]float32{{0, 1}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	after, err := os.ReadFile(outsideLink)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("Upsert wrote through a hard-linked vectors.bin entry: the file outside the index directory changed from %d to %d bytes", len(before), len(after))
	}

	// The index itself must still have grown correctly through the rename.
	if !idx.Contains("one") || !idx.Contains("two") {
		t.Fatal("Upsert did not record both entries in its own vectors.bin")
	}
	results, err := idx.Search([]float32{0, 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SHA != "two" {
		t.Fatalf("results = %+v, want [two]", results)
	}
}
