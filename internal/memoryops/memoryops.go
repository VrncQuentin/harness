// Package memoryops contains semantic-memory operations that sit on top of the
// memory repo, embedder, and episode index. Runtime wires these operations into
// the UI and session manager but does not implement the domain logic itself.
package memoryops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/embedder"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/index"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/retrieval"
	"github.com/vrnc/harness/internal/session"
	"github.com/vrnc/harness/internal/ui"
)

// AfterSaveEmbed returns an AfterSaveFunc that embeds the episode summary and
// updates the project's _episodes index.
func AfterSaveEmbed(embedClient embedder.Client, episodeIndex *EpisodeIndex, repo *gitw.Repo) session.AfterSaveFunc {
	return func(ctx context.Context, result session.SaveResult) error {
		if embedClient == nil || episodeIndex == nil || result.Summary == "" {
			return nil
		}
		chunks := chunkSummary(result.Summary)
		if len(chunks) == 0 {
			return nil
		}

		vectors, err := embedClient.Embed(ctx, chunks)
		if err != nil {
			return fmt.Errorf("embed episode %s: %w", result.ID, err)
		}
		if err := episodeIndex.Upsert(retrieval.EpisodeID(result.EpisodePath), contentHash(result.Summary), vectors); err != nil {
			return fmt.Errorf("index episode %s: %w", result.EpisodePath, err)
		}

		if repo != nil {
			msg := gitw.BuildMessage(
				map[string]string{"type": "index", "episode_id": result.ID},
				"update episode index",
			)
			relVectors := path.Join("index", "_episodes", "vectors.bin")
			relManifest := path.Join("index", "_episodes", "manifest.json")
			if _, err := repo.Commit(msg, []string{relVectors, relManifest}); err != nil {
				slog.Warn("commit index", "err", err)
			}
		}
		return nil
	}
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
func chunkSummary(summary string) []string {
	var chunks []string
	for _, para := range strings.Split(summary, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		chunks = append(chunks, para)
	}
	return chunks
}

// EpisodeScorer implements ui.RetrievalScorer for an episode index. It opens
// lazily so the memory browser can show scores immediately after a fresh-clone
// rebuild creates the index files.
type EpisodeScorer struct {
	Embedder embedder.Client
	Config   config.PromptConfig
	Index    *EpisodeIndex
}

func (s *EpisodeScorer) ScoreEpisodes(ctx context.Context, _, _ string, query string, episodePaths []string) (map[string]ui.RetrievalScore, error) {
	out := make(map[string]ui.RetrievalScore, len(episodePaths))
	for _, p := range episodePaths {
		out[p] = ui.RetrievalScore{}
	}
	if s.Index == nil {
		return out, nil
	}

	for _, p := range episodePaths {
		id := retrieval.EpisodeID(p)
		score := out[p]
		score.Indexed = s.Index.Contains(id)
		out[p] = score
	}
	if strings.TrimSpace(query) == "" || s.Embedder == nil || len(episodePaths) == 0 {
		return out, nil
	}

	vecs, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return out, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return out, nil
	}
	results, err := s.Index.Search(vecs[0], len(episodePaths)*2)
	if err != nil {
		return out, err
	}
	semantic := retrieval.BestSemanticScores(results)
	oldestFirst := append([]string(nil), episodePaths...)
	sort.Strings(oldestFirst)
	scores := retrieval.BlendEpisodeScores(oldestFirst, semantic, s.Config.SemanticWeight, s.Config.RecencyWeight)
	for _, p := range oldestFirst {
		score := out[p]
		score.Score = scores[p]
		score.HasScore = true
		out[p] = score
	}
	return out, nil
}

// EpisodeRebuilder walks episode files, embeds missing episode IDs, updates the
// active project's episode index, and commits the updated index files. The
// operation is idempotent: already-indexed episodes are skipped.
type EpisodeRebuilder struct {
	Mem       memory.Repo
	Embedder  embedder.Client
	Index     *index.Index
	IndexDir  string
	Repo      *gitw.Repo
	OnRebuilt func(*index.Index)
}

func (rb *EpisodeRebuilder) Rebuild(ctx context.Context) error {
	if rb.Index == nil {
		if idx, err := index.Open(rb.IndexDir); err == nil {
			rb.Index = idx
		}
	}

	episodesRoot := "episodes"
	entries, err := rb.Mem.Walk(episodesRoot)
	if err != nil {
		return fmt.Errorf("index rebuild: walk episodes: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Dir || !strings.HasSuffix(e.Path, ".md") {
			continue
		}
		paths = append(paths, e.Path)
	}
	sort.Strings(paths)

	if len(paths) == 0 {
		return nil
	}

	type pending struct {
		path    string
		content string
	}
	var work []pending
	for _, p := range paths {
		body, err := rb.Mem.Read(p)
		if err != nil {
			slog.Warn("index rebuild: skip unreadable episode", "path", p, "err", err)
			continue
		}
		chunks := chunkSummary(string(body))
		if len(chunks) == 0 {
			continue
		}
		work = append(work, pending{path: p, content: string(body)})
	}

	if len(work) == 0 {
		return nil
	}

	allChunks := make([]string, 0)
	chunkCounts := make([]int, len(work))
	for i, w := range work {
		chunks := chunkSummary(w.content)
		allChunks = append(allChunks, chunks...)
		chunkCounts[i] = len(chunks)
	}

	vectors, err := rb.Embedder.Embed(ctx, allChunks)
	if err != nil {
		return fmt.Errorf("index rebuild: embed: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil
	}
	if len(vectors) != len(allChunks) {
		return fmt.Errorf("index rebuild: embed returned %d vectors for %d chunks", len(vectors), len(allChunks))
	}
	dim := len(vectors[0])
	for i, v := range vectors {
		if len(v) != dim {
			return fmt.Errorf("index rebuild: vector %d dimension mismatch: got %d, want %d", i, len(v), dim)
		}
	}
	if rb.Index == nil {
		idx, err := index.Create(rb.IndexDir, dim)
		if err != nil {
			return fmt.Errorf("index rebuild: create index %s: %w", rb.IndexDir, err)
		}
		rb.Index = idx
	}
	if rb.Index.Dim() != dim {
		return fmt.Errorf("index rebuild: dimension mismatch: index has %d, got %d", rb.Index.Dim(), dim)
	}

	offset := 0
	for i, w := range work {
		n := chunkCounts[i]
		if n == 0 {
			continue
		}
		epVecs := vectors[offset : offset+n]
		offset += n
		if err := rb.Index.Upsert(retrieval.EpisodeID(w.path), contentHash(w.content), epVecs); err != nil {
			slog.Warn("index rebuild: add episode", "path", w.path, "err", err)
		}
	}

	if rb.Repo != nil {
		relVectors := path.Join("index", "_episodes", "vectors.bin")
		relManifest := path.Join("index", "_episodes", "manifest.json")
		msg := gitw.BuildMessage(
			map[string]string{"type": "index-rebuild"},
			fmt.Sprintf("rebuild episode index: %d new episodes", len(work)),
		)
		if _, err := rb.Repo.Commit(msg, []string{relVectors, relManifest}); err != nil {
			slog.Warn("index rebuild: commit", "err", err)
		}
	}

	slog.Info("index rebuild complete", "new_episodes", len(work))
	if rb.OnRebuilt != nil {
		rb.OnRebuilt(rb.Index)
	}
	return nil
}

// DedupChecker embeds the candidate text and existing facts, then compares
// cosine similarity.
type DedupChecker struct {
	Mem      memory.Reader
	Embedder embedder.Client
}

func (dc *DedupChecker) CheckSimilar(ctx context.Context, text string, threshold float64) (bool, string, float64, error) {
	if dc.Mem == nil || dc.Embedder == nil {
		return false, "", 0, nil
	}
	existing, err := dc.Mem.Read("facts.md")
	if err != nil || len(existing) == 0 {
		return false, "", 0, nil
	}
	facts := extractFactLines(string(existing))
	if len(facts) == 0 {
		return false, "", 0, nil
	}

	chunks := make([]string, 0, len(facts)+1)
	chunks = append(chunks, text)
	chunks = append(chunks, facts...)

	vectors, err := dc.Embedder.Embed(ctx, chunks)
	if err != nil || len(vectors) == 0 || len(vectors[0]) == 0 {
		return false, "", 0, err
	}
	if len(vectors) != len(chunks) {
		return false, "", 0, fmt.Errorf("embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
	}

	candidate := vectors[0]
	bestScore := 0.0
	bestFact := ""
	for i, v := range vectors[1:] {
		sim := cosineSimilarity(candidate, v)
		if sim > bestScore {
			bestScore = sim
			bestFact = facts[i]
			if len(bestFact) > 120 {
				bestFact = bestFact[:120] + "..."
			}
		}
	}
	if bestScore >= threshold {
		return true, bestFact, bestScore, nil
	}
	return false, "", bestScore, nil
}

func extractFactLines(content string) []string {
	lines := strings.Split(content, "\n")
	facts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-* \t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		facts = append(facts, line)
	}
	return facts
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
