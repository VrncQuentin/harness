package memoryops

import (
	"errors"
	"fmt"
	"io/fs"
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

// EpisodeIndex owns the synchronized index handle for one project. The
// index directory is opened through a rootfs Anchor derived from the
// repository's pinned handle, so containment and identity are guaranteed.
type EpisodeIndex struct {
	mu     sync.Mutex
	dir    string
	anchor *rootfs.Anchor
	idx    *index.Index
}

// NewEpisodeIndex verifies that anchor and dir refer to the same directory
// and opens an existing index.  The caller must have established the anchor
// through the repository's DirReader.SubAnchor, guaranteeing containment.
func NewEpisodeIndex(anchor *rootfs.Anchor, dir string) (*EpisodeIndex, error) {
	if anchor == nil {
		return nil, errors.New("episode index: anchor is nil")
	}
	if err := sameDir(anchor, dir); err != nil {
		return nil, err
	}
	idx, idxErr := index.Open(dir)
	if idxErr != nil && !errors.Is(idxErr, fs.ErrNotExist) {
		return nil, idxErr
	}
	return &EpisodeIndex{dir: dir, anchor: anchor, idx: idx}, nil
}

func sameDir(anchor *rootfs.Anchor, dir string) error {
	r, err := rootfs.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("episode index: open dir %s: %w", dir, err)
	}
	defer func() { _ = r.Close() }()
	same, err := anchor.SameRoot(r)
	if err != nil {
		return fmt.Errorf("episode index: compare anchor and dir %s: %w", dir, err)
	}
	if !same {
		return fmt.Errorf("episode index: anchor and dir %s identify different directories", dir)
	}
	return nil
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
	if err := e.verify(); err != nil {
		return nil, err
	}
	return idx.Search(query, k)
}

// Contains reports whether source has a current entry in the shared index.
func (e *EpisodeIndex) Contains(source string) bool {
	e.mu.Lock()
	idx := e.idx
	e.mu.Unlock()
	if idx == nil {
		return false
	}
	if err := e.verify(); err != nil {
		return false
	}
	return idx.Contains(source)
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
	if err := e.verify(); err != nil {
		return err
	}
	if e.idx == nil {
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
