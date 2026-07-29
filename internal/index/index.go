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
	"os"
	"path/filepath"
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
type Index struct {
	mu       sync.Mutex
	dir      string
	dim      int
	manifest Manifest
}

// Open reads an existing index at dir, or returns a zero-vector
// ErrNotExist when the directory or files are missing.
func Open(dir string) (*Index, error) {
	idx := &Index{dir: dir}
	mf, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("index: open %s: %w", dir, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("index: read manifest %s: %w", dir, err)
	}
	if err := json.Unmarshal(mf, &idx.manifest); err != nil {
		return nil, fmt.Errorf("index: parse manifest %s: %w", dir, err)
	}
	if err := validateManifest(dir, idx.manifest); err != nil {
		return nil, err
	}
	idx.dim = idx.manifest.Dim
	return idx, nil
}

// Create initializes a new index at dir. Existing indices are overwritten.
func Create(dir string, dim int) (*Index, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("index: invalid dimension %d", dim)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir %s: %w", dir, err)
	}
	idx := &Index{
		dir: dir,
		dim: dim,
		manifest: Manifest{
			Dim:    dim,
			Chunks: nil,
		},
	}
	if err := writeFileAtomic(filepath.Join(dir, vectorsFile), nil, 0o644); err != nil {
		return nil, fmt.Errorf("index: write vectors %s: %w", dir, err)
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

// Upsert stores vectors for source.  Vectors are assembled via copy-on-write
// and published through a pinned root.  The old manifest is kept valid until
// both publications succeed.
func (idx *Index) Upsert(source, contentHash string, vectors [][]float32) error {
	return idx.upsert(source, contentHash, vectors, nil)
}

// upsert is Upsert with an optional manifest-write function.  Tests inject
// a writer that returns an error to exercise manifest-publication failure
// without blocking vector publication.
func (idx *Index) upsert(source, contentHash string, vectors [][]float32, writeManifest func(root *rootfs.Root, data []byte) error) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Build the next manifest without mutating idx.manifest.
	next := Manifest{Dim: idx.manifest.Dim, Count: idx.manifest.Count}
	next.Chunks = make([]Entry, len(idx.manifest.Chunks))
	copy(next.Chunks, idx.manifest.Chunks)

	for i, e := range next.Chunks {
		if e.Source == source || (e.Source == "" && e.SHA == source) {
			if e.ContentHash == contentHash && contentHash != "" {
				return nil
			}
			next.Count -= e.Length
			next.Chunks = append(next.Chunks[:i], next.Chunks[i+1:]...)
			break
		}
	}

	for _, v := range vectors {
		if len(v) != idx.dim {
			return fmt.Errorf("index: dimension mismatch: got %d, want %d", len(v), idx.dim)
		}
	}

	// Pin the index directory first — every subsequent operation uses
	// this handle so a re-point between read and publish is refused.
	root, err := rootfs.Open(idx.dir)
	if err != nil {
		return fmt.Errorf("index: open root: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Read existing vectors through the pinned root to derive the offset.
	oldVectors, err := root.ReadFile(vectorsFile)
	if err != nil {
		return fmt.Errorf("index: read vectors: %w", err)
	}
	offset := int64(len(oldVectors))

	// Assemble the complete vectors file.
	newLen := len(oldVectors) + len(vectors)*idx.dim*4
	newVectors := make([]byte, newLen)
	copy(newVectors, oldVectors)
	pos := len(oldVectors)
	for _, v := range vectors {
		for _, val := range v {
			binary.LittleEndian.PutUint32(newVectors[pos:], math.Float32bits(val))
			pos += 4
		}
	}

	// Publish vectors first via WriteStreamAtomic. If this succeeds and
	// a subsequent manifest publication fails, the old manifest (still
	// in effect) only references the old prefix; the unreferenced tail
	// bytes at the end of vectors are harmless.
	if err := root.WriteStreamAtomic(vectorsFile, bytes.NewReader(newVectors), 0o644); err != nil {
		return fmt.Errorf("index: publish vectors: %w", err)
	}

	// Build the updated manifest and publish it.
	next.Chunks = append(next.Chunks, Entry{
		SHA: source, Source: source, ContentHash: contentHash,
		Offset: offset, Length: len(vectors),
	})
	next.Count += len(vectors)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}

	wm := writeManifest
	if wm == nil {
		wm = func(r *rootfs.Root, d []byte) error {
			return r.WriteStreamAtomic(manifestFile, bytes.NewReader(d), 0o644)
		}
	}
	if err := wm(root, data); err != nil {
		return fmt.Errorf("index: publish manifest: %w", err)
	}
	// Both publications succeeded — commit the new manifest.
	idx.manifest = next
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
	f, err := os.Open(filepath.Join(idx.dir, vectorsFile))
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

func (idx *Index) writeManifest() error {
	data, err := json.MarshalIndent(idx.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(idx.dir, manifestFile), data, 0o644); err != nil {
		return fmt.Errorf("index: write manifest: %w", err)
	}
	return nil
}

func validateManifest(dir string, manifest Manifest) error {
	if manifest.Dim <= 0 {
		return fmt.Errorf("index: invalid manifest dimension %d in %s", manifest.Dim, dir)
	}
	if manifest.Count < 0 {
		return fmt.Errorf("index: invalid manifest count %d in %s", manifest.Count, dir)
	}
	info, err := os.Stat(filepath.Join(dir, vectorsFile))
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

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return err
		}
		if retryErr := os.Rename(tmpName, path); retryErr != nil {
			return retryErr
		}
	}
	return nil
}
