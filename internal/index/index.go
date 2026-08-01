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
	"io/fs"
	"math"
	"sort"
	"sync"

	"github.com/VrncQuentin/harness/internal/pathid"
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
	lockKey  string
	dir      string
	dim      int
	manifest Manifest
}

// indexMutationLocks serializes index mutations per physical index directory.
//
// idx.mu cannot do this job alone: a handle is opened per episode save or per
// tool call, so two concurrent writers against the same index hold two
// different per-instance mutexes and exclude nothing.  vectors.bin and
// manifest.json are shared filesystem state, so the lock has to be keyed by
// the physical directory rather than by the handle — two handles to one
// repository, however each was spelled, must land on one lock.
//
// The key is the physical identity from internal/pathid, not a pathname.
// Keying on a pathname would let a junction alias or a differently-cased
// spelling of the same directory hand out a second, separate mutex, and the
// two handles would serialize against nothing.
//
// This guards harness-internal concurrency only.  A process writing the same
// index at the same moment is outside its reach.
var indexMutationLocks sync.Map // lock key -> *sync.Mutex

// lockForMutation takes the per-directory coordinator lock and this handle's
// mutex, and returns the function that releases both in the right order.
func (idx *Index) lockForMutation() func() {
	actual, _ := indexMutationLocks.LoadOrStore(idx.lockKey, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		// Unreachable: only *sync.Mutex is ever stored.
		lock = &sync.Mutex{}
	}
	lock.Lock()
	idx.mu.Lock()
	return func() {
		idx.mu.Unlock()
		lock.Unlock()
	}
}

// OpenRooted reads an index through a pinned Root handle instead of by
// pathname.  The caller owns the Root; OpenRooted does not close it.
func OpenRooted(root *rootfs.Root, dir string) (*Index, error) {
	lockKey, err := pathid.LockKey(dir)
	if err != nil {
		return nil, fmt.Errorf("index: identify %s: %w", dir, err)
	}
	manifest, err := readManifestRooted(root, dir)
	if err != nil {
		return nil, err
	}
	return &Index{lockKey: lockKey, dir: dir, dim: manifest.Dim, manifest: manifest}, nil
}

// CreateRooted initializes a new index through a pinned Root handle.
func CreateRooted(root *rootfs.Root, dir string, dim int) (*Index, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("index: invalid dimension %d", dim)
	}
	if err := root.MkdirAll(".", 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir %s: %w", dir, err)
	}
	lockKey, err := pathid.LockKey(dir)
	if err != nil {
		return nil, fmt.Errorf("index: identify %s: %w", dir, err)
	}
	idx := &Index{
		lockKey: lockKey,
		dir:     dir,
		dim:     dim,
		manifest: Manifest{
			Dim:    dim,
			Chunks: nil,
		},
	}
	if err := root.WriteStreamAtomic(vectorsFile, bytes.NewReader(nil), 0o644); err != nil {
		return nil, fmt.Errorf("index: write vectors %s: %w", dir, err)
	}
	if err := idx.writeManifestRooted(root); err != nil {
		return nil, err
	}
	return idx, nil
}

// readManifestRooted reads, parses, and validates the manifest through the
// pinned root.  It is the committed on-disk state a transaction starts from.
func readManifestRooted(root *rootfs.Root, dir string) (Manifest, error) {
	mf, err := root.ReadFile(manifestFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Manifest{}, fmt.Errorf("index: open %s: %w", dir, fs.ErrNotExist)
		}
		return Manifest{}, fmt.Errorf("index: read manifest %s: %w", dir, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(mf, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("index: parse manifest %s: %w", dir, err)
	}
	if err := validateManifestRooted(root, dir, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (idx *Index) writeManifestRooted(root *rootfs.Root) error {
	data, err := json.MarshalIndent(idx.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	return publishManifestData(root, data, rootfs.WriteHooks{})
}

// publishManifestData publishes serialized manifest data through the pinned
// root.  Production callers pass no hooks; tests inject them at the real
// WriteStreamAtomic lifecycle points so a regression can be observed without
// replacing the writer.
func publishManifestData(root *rootfs.Root, data []byte, hooks rootfs.WriteHooks) error {
	var werr error
	if hooks.AfterOpen == nil && hooks.Sync == nil && hooks.BeforeRename == nil {
		werr = root.WriteStreamAtomic(manifestFile, bytes.NewReader(data), 0o644)
	} else {
		werr = root.WriteStreamAtomicWithHooks(manifestFile, bytes.NewReader(data), 0o644, hooks)
	}
	if werr != nil {
		return fmt.Errorf("index: write manifest: %w", werr)
	}
	return nil
}

// SearchRooted reads vectors through a pinned Root handle.
func (idx *Index) SearchRooted(root *rootfs.Root, query []float32, k int) ([]Result, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(query) != idx.dim {
		return nil, fmt.Errorf("index: query dim mismatch: got %d, want %d", len(query), idx.dim)
	}
	data, err := root.ReadFile(vectorsFile)
	if err != nil {
		return nil, fmt.Errorf("index: read vectors: %w", err)
	}

	type scored struct {
		sha   string
		score float32
	}
	var results []scored

	for _, entry := range idx.manifest.Chunks {
		entryEnd, err := entryRange(entry.Offset, entry.Length, idx.dim, int64(len(data)))
		if err != nil {
			return nil, err
		}
		vecs := make([]float32, int64(entry.Length)*int64(idx.dim))
		r := bytes.NewReader(data[entry.Offset:entryEnd])
		if err := binary.Read(r, binary.LittleEndian, &vecs); err != nil {
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

// UpsertRooted stores vectors and publishes through a pinned Root handle.
func (idx *Index) UpsertRooted(root *rootfs.Root, source, contentHash string, vectors [][]float32) error {
	return idx.upsertRooted(root, source, contentHash, vectors, rootfs.WriteHooks{})
}

func (idx *Index) upsertRooted(root *rootfs.Root, source, contentHash string, vectors [][]float32, hooks rootfs.WriteHooks) error {
	unlock := idx.lockForMutation()
	defer unlock()

	// Start from the committed on-disk state, not the in-memory copy.  The
	// in-memory copy can trail the disk when another handle published since
	// this one was opened; re-reading under the coordinator lock is what
	// makes a write a transaction that never rolls a sibling's commit back.
	committed, err := readManifestRooted(root, idx.dir)
	if err != nil {
		return err
	}
	idx.manifest = committed
	idx.dim = committed.Dim

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

	oldVectors, err := root.ReadFile(vectorsFile)
	if err != nil {
		return fmt.Errorf("index: read vectors: %w", err)
	}
	offset := int64(len(oldVectors))

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

	if err := root.WriteStreamAtomic(vectorsFile, bytes.NewReader(newVectors), 0o644); err != nil {
		return fmt.Errorf("index: publish vectors: %w", err)
	}

	next.Chunks = append(next.Chunks, Entry{
		SHA: source, Source: source, ContentHash: contentHash,
		Offset: offset, Length: len(vectors),
	})
	next.Count += len(vectors)

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	if err := publishManifestData(root, data, hooks); err != nil {
		return err
	}
	idx.manifest = next
	return nil
}

func validateManifestRooted(root *rootfs.Root, dir string, manifest Manifest) error {
	if manifest.Dim <= 0 {
		return fmt.Errorf("index: invalid manifest dimension %d in %s", manifest.Dim, dir)
	}
	if manifest.Count < 0 {
		return fmt.Errorf("index: invalid manifest count %d in %s", manifest.Count, dir)
	}
	info, err := root.Lstat(vectorsFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("index: manifest exists but vectors are missing in %s", dir)
		}
		return fmt.Errorf("index: stat vectors %s: %w", dir, err)
	}
	if info.IsDir() {
		return fmt.Errorf("index: vectors path is a directory in %s", dir)
	}
	fileSize := info.Size()
	stride := int64(manifest.Dim) * 4
	if stride/4 != int64(manifest.Dim) {
		return fmt.Errorf("index: stride overflow for dim %d in %s", manifest.Dim, dir)
	}
	if fileSize%stride != 0 {
		return fmt.Errorf("index: vector file size %d is not aligned to dimension %d in %s", fileSize, manifest.Dim, dir)
	}
	count := 0
	for i, entry := range manifest.Chunks {
		if entry.SHA == "" {
			return fmt.Errorf("index: manifest entry %d has empty sha in %s", i, dir)
		}
		if entry.Offset%stride != 0 {
			return fmt.Errorf("index: manifest entry %d offset %d is not vector-aligned in %s", i, entry.Offset, dir)
		}
		_, err := entryRange(entry.Offset, entry.Length, manifest.Dim, fileSize)
		if err != nil {
			return fmt.Errorf("index: %w in %s", err, dir)
		}
		count += entry.Length
	}
	if count != manifest.Count {
		return fmt.Errorf("index: manifest count %d does not match chunk lengths %d in %s", manifest.Count, count, dir)
	}
	return nil
}

func entryRange(offset int64, length, dim int, fileSize int64) (end int64, err error) {
	if length < 0 {
		return 0, fmt.Errorf("manifest entry has negative length %d", length)
	}
	if dim <= 0 {
		return 0, fmt.Errorf("manifest entry has non-positive dim %d", dim)
	}
	stride := int64(dim) * 4
	if stride/4 != int64(dim) {
		return 0, fmt.Errorf("manifest entry stride overflow for dim %d", dim)
	}
	byteLen := int64(length) * stride
	if byteLen/stride != int64(length) {
		return 0, fmt.Errorf("manifest entry byte length overflow for length %d, dim %d", length, dim)
	}
	if offset < 0 {
		return 0, fmt.Errorf("manifest entry has negative offset %d", offset)
	}
	end = offset + byteLen
	if end < offset {
		return 0, fmt.Errorf("manifest entry end overflow for offset %d, byteLen %d", offset, byteLen)
	}
	if end > fileSize {
		return 0, fmt.Errorf("manifest entry range [%d:%d] exceeds file size %d", offset, end, fileSize)
	}
	return end, nil
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
