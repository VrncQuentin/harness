package memoryops

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sync"

	"github.com/VrncQuentin/harness/internal/index"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

const (
	EpisodeIndexRootRel     = "index/_episodes"
	EpisodeIndexVectorsRel  = "index/_episodes/vectors.bin"
	EpisodeIndexManifestRel = "index/_episodes/manifest.json"
)

// EpisodeIndexCommitPaths returns repo-relative paths touched by episode index updates.
func EpisodeIndexCommitPaths() []string {
	return []string{path.Clean(EpisodeIndexVectorsRel), path.Clean(EpisodeIndexManifestRel)}
}

// EpisodeIndex owns the synchronized index handle for one project. Every
// retrieval and mutation path shares this handle, so newly saved episodes are
// visible immediately without a runtime restart.
//
// It owns the pinned index directory for its own lifetime. The index files are
// written lazily — not until the first embedding succeeds — so the directory
// handle has to outlive construction, and tying its lifetime to whichever
// caller happened to build this would make the index's correctness depend on
// somebody else's Close.
type EpisodeIndex struct {
	mu  sync.Mutex
	dir *rootfs.Root
	rel string
	idx *index.Index
}

// NewEpisodeIndex opens the episode index at rel inside repo, a pinned project
// memory repo. rel is resolved through the repo's handle, so the directory the
// index ends up writing to is inside the repo by construction rather than by a
// comparison. A missing index is created on the first successful embedding;
// a malformed one is returned to the caller instead of being mistaken for a
// missing one. The caller closes the result.
func NewEpisodeIndex(repo memory.SubRooter, rel string) (*EpisodeIndex, error) {
	dir, err := repo.SubRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("episode index: pin %s: %w", rel, err)
	}
	idx, err := index.Open(dir, rel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = dir.Close()
		return nil, err
	}
	return &EpisodeIndex{dir: dir, rel: rel, idx: idx}, nil
}

// Close releases the pinned index directory.
func (e *EpisodeIndex) Close() error {
	e.mu.Lock()
	e.idx = nil
	e.mu.Unlock()
	if err := e.dir.Close(); err != nil {
		return fmt.Errorf("episode index: close %s: %w", e.rel, err)
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
		idx, err := index.Create(e.dir, e.rel, dim)
		if err != nil {
			return fmt.Errorf("episode index: create %s: %w", e.rel, err)
		}
		e.idx = idx
	}
	if e.idx.Dim() != dim {
		return fmt.Errorf("episode index: dimension mismatch: index has %d, got %d", e.idx.Dim(), dim)
	}
	return e.idx.Upsert(source, contentHash, vectors)
}

// ContainsCurrent reports whether source is present with the supplied content
// hash. A missing index contains nothing.
func (e *EpisodeIndex) ContainsCurrent(source, contentHash string) bool {
	e.mu.Lock()
	idx := e.idx
	e.mu.Unlock()
	return idx != nil && idx.ContainsCurrent(source, contentHash)
}

// Ready reports whether an index exists on disk yet.
func (e *EpisodeIndex) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.idx != nil
}
