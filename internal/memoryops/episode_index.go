package memoryops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/VrncQuentin/harness/internal/index"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

const (
	EpisodeIndexRootRel     = "index/_episodes"
	EpisodeIndexVectorsRel  = "index/_episodes/vectors.bin"
	EpisodeIndexManifestRel = "index/_episodes/manifest.json"
)

// EpisodeIndexDir returns the filesystem path to a project's episode index.
func EpisodeIndexDir(projectRoot string) string {
	return filepath.Join(projectRoot, filepath.FromSlash(EpisodeIndexRootRel))
}

// EpisodeIndexCommitPaths returns repo-relative paths touched by episode index updates.
func EpisodeIndexCommitPaths() []string {
	return []string{path.Clean(EpisodeIndexVectorsRel), path.Clean(EpisodeIndexManifestRel)}
}

// EpisodeIndex owns the synchronized index handle for one project. Every
// retrieval and mutation path shares this handle, so newly saved episodes are
// visible immediately without a runtime restart. The index directory is pinned
// through a rootfs Anchor so repointing or escaping is detected.
type EpisodeIndex struct {
	mu     sync.Mutex
	dir    string
	anchor *rootfs.Anchor
	idx    *index.Index
}

// NewEpisodeIndex pins the index directory at dir through a rootfs Anchor
// and opens an existing index. If the directory does not exist it is created
// before pinning. A missing index after creation means no episodes have been
// embedded yet; the caller creates the index lazily on first Upsert.
func NewEpisodeIndex(dir string) (*EpisodeIndex, error) {
	a, err := rootfs.NewAnchor(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if mkerr := os.MkdirAll(dir, 0o755); mkerr != nil {
				return nil, fmt.Errorf("episode index: mkdir %s: %w", dir, mkerr)
			}
			a, err = rootfs.NewAnchor(dir)
		}
		if err != nil {
			return nil, fmt.Errorf("episode index: pin %s: %w", dir, err)
		}
	}
	idx, err := index.Open(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = a.Close()
		return nil, err
	}
	return &EpisodeIndex{dir: dir, anchor: a, idx: idx}, nil
}

// Search implements index.Searcher. A missing index produces no semantic
// results; callers retain their recency-only fallback until the first save.
func (e *EpisodeIndex) Search(query []float32, k int) ([]index.Result, error) {
	e.mu.Lock()
	idx := e.idx
	e.mu.Unlock()
	if idx == nil {
		return nil, nil
	}
	return idx.Search(query, k)
}

// Contains reports whether source has a current entry in the shared index.
func (e *EpisodeIndex) Contains(source string) bool {
	e.mu.Lock()
	idx := e.idx
	e.mu.Unlock()
	return idx != nil && idx.Contains(source)
}

// Upsert creates the index on first use and replaces obsolete vectors for a
// changed source document.
func (e *EpisodeIndex) Upsert(source, contentHash string, vectors [][]float32) error {
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil
	}
	dim := len(vectors[0])
	for i, vector := range vectors {
		if len(vector) != dim {
			return fmt.Errorf("episode index: vector %d dimension mismatch: got %d, want %d", i, len(vector), dim)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.idx == nil {
		if err := e.verify(); err != nil {
			return err
		}
		idx, err := index.Create(e.dir, dim)
		if err != nil {
			return fmt.Errorf("episode index: create %s: %w", e.dir, err)
		}
		e.idx = idx
	}
	if e.idx.Dim() != dim {
		return fmt.Errorf("episode index: dimension mismatch: index has %d, got %d", e.idx.Dim(), dim)
	}
	return e.idx.Upsert(source, contentHash, vectors)
}

// verify confirms the pinned directory has not been replaced or repointed.
func (e *EpisodeIndex) verify() error {
	r, err := e.anchor.Open()
	if err != nil {
		return fmt.Errorf("episode index: verify %s: %w", e.dir, err)
	}
	_ = r.Close()
	return nil
}

// Current returns the shared concrete handle for migration and test helpers.
func (e *EpisodeIndex) Current() *index.Index {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.idx
}

// Replace adopts a handle created by the explicit rebuild workflow.
func (e *EpisodeIndex) Replace(idx *index.Index) {
	e.mu.Lock()
	e.idx = idx
	e.mu.Unlock()
}

// Close releases the pinned directory handle.
func (e *EpisodeIndex) Close() error {
	return e.anchor.Close()
}
