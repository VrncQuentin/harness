// Package index manages flat vector indices stored as (vectors.bin,
// manifest.json) pairs under a project's index/ tree. Each indexable
// tree (episodes, a project directory) gets its own subdirectory.
package index

import (
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
)

const (
	vectorsFile  = "vectors.bin"
	manifestFile = "manifest.json"
)

// Entry is one record in the manifest.
type Entry struct {
	SHA    string `json:"sha"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
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
	idx.dim = idx.manifest.Dim
	return idx, nil
}

// Create initializes a new index at dir. Existing indices are overwritten.
func Create(dir string, dim int) (*Index, error) {
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
	return idx, nil
}

// Add appends vectors for the given SHA. Returns error if the SHA already
// exists (idempotent check). The vectors slice must match the index
// dimension. Safe for concurrent use.
func (idx *Index) Add(sha string, vectors [][]float32) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, e := range idx.manifest.Chunks {
		if e.SHA == sha {
			return nil // already indexed
		}
	}
	offset := idx.currentFileSize()
	for _, v := range vectors {
		if len(v) != idx.dim {
			return fmt.Errorf("index: dimension mismatch: got %d, want %d", len(v), idx.dim)
		}
	}
	idx.manifest.Chunks = append(idx.manifest.Chunks, Entry{
		SHA:    sha,
		Offset: offset,
		Length: len(vectors),
	})
	idx.manifest.Count += len(vectors)

	if err := idx.appendVectors(vectors); err != nil {
		return err
	}
	return idx.writeManifest()
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
			score := cosineSimilarity(query, chunk)
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

// currentFileSize returns the size of vectors.bin, or 0 if it doesn't exist.
func (idx *Index) currentFileSize() int64 {
	info, err := os.Stat(filepath.Join(idx.dir, vectorsFile))
	if err != nil {
		return 0
	}
	return info.Size()
}

func (idx *Index) appendVectors(vectors [][]float32) error {
	f, err := os.OpenFile(filepath.Join(idx.dir, vectorsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("index: open vectors for append: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, len(vectors)*idx.dim*4)
	pos := 0
	for _, v := range vectors {
		for _, val := range v {
			binary.LittleEndian.PutUint32(buf[pos:], math.Float32bits(val))
			pos += 4
		}
	}
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("index: write vectors: %w", err)
	}
	return nil
}

func (idx *Index) writeManifest() error {
	data, err := json.MarshalIndent(idx.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(idx.dir, manifestFile), data, 0o644); err != nil {
		return fmt.Errorf("index: write manifest: %w", err)
	}
	return nil
}

// cosineSimilarity returns the cosine similarity between a and b.
func cosineSimilarity(a, b []float32) float32 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(normA*normB))
}
