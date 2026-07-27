// Package index manages flat vector indices stored as (vectors.bin,
// manifest.json) pairs under a project's index/ tree. Each indexable
// tree (episodes, a project directory) gets its own subdirectory.
package index

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sort"
	"sync"

	"github.com/VrncQuentin/harness/internal/rootfs"
	"github.com/VrncQuentin/harness/internal/vector"
)

const (
	vectorsFile  = "vectors.bin"
	manifestFile = "manifest.json"
)

// Entry is one record in the manifest.
type Entry struct {
	// SHA is the stable result identifier. It is retained for on-disk
	// compatibility; episode indexes use the repo-relative source path here.
	SHA         string `json:"sha"`
	Source      string `json:"source,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Offset      int64  `json:"offset"`
	Length      int    `json:"length"`
}

// Manifest is the index header mapping content SHAs to vector offsets.
type Manifest struct {
	Dim    int     `json:"dim"`
	Count  int     `json:"count"`
	Chunks []Entry `json:"chunks"`
}

// Result is a single ANN search result.
type Result struct {
	SHA   string
	Score float32
}

// Searcher is the read-only index surface used by retrieval consumers.
type Searcher interface {
	Search(query []float32, k int) ([]Result, error)
}

// Index manages one vectors.bin + manifest.json pair on disk. Safe for
// concurrent use.
//
// The index is addressed through a directory handle, not a pathname, and the
// handle is one the caller obtained by resolving the index's location through
// the project memory repo's own handle. That distinction is the whole point:
// pinning "<repo>/index/_episodes" as a name pins whatever the name leads to,
// and "index" or "_episodes" can already be a symlink or a junction pointing
// out of the repository — the pin would then be perfectly sound and perfectly
// outside the repo. Resolving the relative location through the repo's handle
// is what makes "inside the repo" true by construction.
//
// The handle is borrowed, not owned: whoever pinned the directory closes it,
// and it must outlive the Index.
type Index struct {
	mu sync.Mutex
	// dir is the pinned index directory.
	dir *rootfs.Root
	// name is the index's repo-relative location, for diagnostics only. It is
	// never resolved.
	name     string
	dim      int
	manifest Manifest
}

// Open reads the existing index in the pinned directory dir, or returns an
// error satisfying errors.Is(err, fs.ErrNotExist) when its files are missing.
// name labels the index in diagnostics and is never resolved.
func Open(dir *rootfs.Root, name string) (*Index, error) {
	idx := &Index{dir: dir, name: name}
	mf, err := dir.ReadFile(manifestFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("index: open %s: %w", name, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("index: read manifest %s: %w", name, err)
	}
	if err := json.Unmarshal(mf, &idx.manifest); err != nil {
		return nil, fmt.Errorf("index: parse manifest %s: %w", name, err)
	}
	if err := idx.validateManifest(idx.manifest); err != nil {
		return nil, err
	}
	idx.dim = idx.manifest.Dim
	return idx, nil
}

// Create initializes a new index in the pinned directory dir. Existing indices
// are overwritten. name labels the index in diagnostics.
func Create(dir *rootfs.Root, name string, dim int) (*Index, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("index: invalid dimension %d", dim)
	}
	idx := &Index{
		dir:  dir,
		name: name,
		dim:  dim,
		manifest: Manifest{
			Dim:    dim,
			Chunks: nil,
		},
	}
	if err := idx.dir.WriteStreamAtomic(vectorsFile, bytes.NewReader(nil), 0o644); err != nil {
		return nil, fmt.Errorf("index: write vectors %s: %w", name, err)
	}
	if err := idx.writeManifest(); err != nil {
		return nil, err
	}
	return idx, nil
}

// Add appends vectors for the given SHA. Existing SHAs are idempotent no-ops.
// The vectors slice must match the index dimension. Safe for concurrent use.
func (idx *Index) Add(sha string, vectors [][]float32) error {
	return idx.Upsert(sha, sha, vectors)
}

// Upsert stores vectors for source. A matching source and content hash is a
// no-op; changed source content replaces its old vectors. Result identifiers
// are the source path, which makes equal episode basenames in different agent
// directories distinct.
// The vector file is opened once and held for the whole sequence — measure the
// current end, append there, and on a manifest failure truncate back to where
// it started. Reopening the file between those steps would let the length that
// was measured, the bytes that were written, and the length rolled back to
// belong to three different files; holding one handle makes the rollback undo
// this call's own append and nothing else.
func (idx *Index) Upsert(source, contentHash string, vectors [][]float32) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	previous := idx.manifest
	for i, e := range idx.manifest.Chunks {
		if e.Source == source || (e.Source == "" && e.SHA == source) {
			if e.ContentHash == contentHash && contentHash != "" {
				return nil
			}
			idx.manifest.Count -= e.Length
			idx.manifest.Chunks = append(idx.manifest.Chunks[:i], idx.manifest.Chunks[i+1:]...)
			break
		}
	}
	for _, v := range vectors {
		if len(v) != idx.dim {
			idx.manifest = previous
			return fmt.Errorf("index: dimension mismatch: got %d, want %d", len(v), idx.dim)
		}
	}

	f, err := idx.dir.OpenReadWrite(vectorsFile, 0o644)
	if err != nil {
		idx.manifest = previous
		return fmt.Errorf("index: open vectors: %w", err)
	}
	defer func() { _ = f.Close() }()

	offset, err := f.Size()
	if err != nil {
		idx.manifest = previous
		return fmt.Errorf("index: stat vectors: %w", err)
	}
	entry := Entry{SHA: source, Source: source, ContentHash: contentHash, Offset: offset, Length: len(vectors)}
	if err := appendVectors(f, vectors, idx.dim); err != nil {
		idx.manifest = previous
		return err
	}
	idx.manifest.Chunks = append(idx.manifest.Chunks, entry)
	idx.manifest.Count += len(vectors)
	if err := idx.writeManifest(); err != nil {
		idx.manifest = previous
		if truncateErr := rollbackVectors(f, offset); truncateErr != nil {
			return fmt.Errorf("%w; rollback vectors: %v", err, truncateErr)
		}
		return err
	}
	return nil
}

// Search performs a flat cosine-similarity scan across all vectors and
// returns the top-k results by descending score. Safe for concurrent use.
func (idx *Index) Search(query []float32, k int) ([]Result, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(query) != idx.dim {
		return nil, fmt.Errorf("index: query dim mismatch: got %d, want %d", len(query), idx.dim)
	}
	f, err := idx.dir.OpenRead(vectorsFile)
	if err != nil {
		return nil, fmt.Errorf("index: open vectors: %w", err)
	}
	defer func() { _ = f.Close() }()

	type scored struct {
		sha   string
		score float32
	}
	var results []scored

	for _, entry := range idx.manifest.Chunks {
		vecs := make([]float32, entry.Length*idx.dim)
		if err := binary.Read(io.NewSectionReader(f, entry.Offset, int64(entry.Length*idx.dim*4)), binary.LittleEndian, &vecs); err != nil {
			return nil, fmt.Errorf("index: read vectors at %d: %w", entry.Offset, err)
		}
		for i := 0; i < entry.Length; i++ {
			chunk := vecs[i*idx.dim : (i+1)*idx.dim]
			score := float32(vector.CosineSimilarity(query, chunk))
			results = append(results, scored{sha: entry.SHA, score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if k > len(results) {
		k = len(results)
	}
	out := make([]Result, k)
	for i := 0; i < k; i++ {
		out[i] = Result{SHA: results[i].sha, Score: results[i].score}
	}
	return out, nil
}

// Dim returns the vector dimension for this index.
func (idx *Index) Dim() int { return idx.dim }

// Contains reports whether sha is present in the index manifest.
func (idx *Index) Contains(sha string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, e := range idx.manifest.Chunks {
		if e.SHA == sha {
			return true
		}
	}
	return false
}

// ContainsCurrent reports whether source is present with the supplied content
// hash. Empty hashes never match: callers should provide a real content digest
// before using this as a rebuild skip check.
func (idx *Index) ContainsCurrent(source, contentHash string) bool {
	if contentHash == "" {
		return false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, e := range idx.manifest.Chunks {
		if (e.Source == source || (e.Source == "" && e.SHA == source)) && e.ContentHash == contentHash {
			return true
		}
	}
	return false
}

// appendVectors writes the vectors at the end of the open file and fsyncs.
func appendVectors(f *rootfs.File, vectors [][]float32, dim int) error {
	buf := make([]byte, len(vectors)*dim*4)
	pos := 0
	for _, v := range vectors {
		for _, val := range v {
			binary.LittleEndian.PutUint32(buf[pos:], math.Float32bits(val))
			pos += 4
		}
	}
	if err := f.Append(buf); err != nil {
		return fmt.Errorf("index: write vectors: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("index: sync vectors: %w", err)
	}
	return nil
}

// rollbackVectors undoes an append through the same handle it was made on.
func rollbackVectors(f *rootfs.File, size int64) error {
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("index: truncate vectors: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("index: sync vectors rollback: %w", err)
	}
	return nil
}

func (idx *Index) writeManifest() error {
	data, err := json.MarshalIndent(idx.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	if err := idx.dir.WriteStreamAtomic(manifestFile, bytes.NewReader(data), 0o644); err != nil {
		return fmt.Errorf("index: write manifest: %w", err)
	}
	return nil
}

func (idx *Index) validateManifest(manifest Manifest) error {
	dir := idx.name
	if manifest.Dim <= 0 {
		return fmt.Errorf("index: invalid manifest dimension %d in %s", manifest.Dim, dir)
	}
	if manifest.Count < 0 {
		return fmt.Errorf("index: invalid manifest count %d in %s", manifest.Count, dir)
	}
	info, err := idx.dir.Stat(vectorsFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("index: manifest exists but vectors are missing in %s", dir)
		}
		return fmt.Errorf("index: stat vectors %s: %w", dir, err)
	}
	if info.IsDir() {
		return fmt.Errorf("index: vectors path is a directory in %s", dir)
	}
	stride := int64(manifest.Dim * 4)
	if info.Size()%stride != 0 {
		return fmt.Errorf("index: vector file size %d is not aligned to dimension %d in %s", info.Size(), manifest.Dim, dir)
	}
	count := 0
	for i, entry := range manifest.Chunks {
		if entry.SHA == "" {
			return fmt.Errorf("index: manifest entry %d has empty sha in %s", i, dir)
		}
		if entry.Offset < 0 || entry.Length < 0 {
			return fmt.Errorf("index: manifest entry %d has invalid offset/length in %s", i, dir)
		}
		if entry.Offset%stride != 0 {
			return fmt.Errorf("index: manifest entry %d offset %d is not vector-aligned in %s", i, entry.Offset, dir)
		}
		bytes := int64(entry.Length) * stride
		if entry.Offset+bytes > info.Size() {
			return fmt.Errorf("index: manifest entry %d extends past vectors file in %s", i, dir)
		}
		count += entry.Length
	}
	if count != manifest.Count {
		return fmt.Errorf("index: manifest count %d does not match chunk lengths %d in %s", manifest.Count, count, dir)
	}
	return nil
}
