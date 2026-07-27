// Package memoryops contains semantic-memory operations that sit on top of the
// memory repo, embedder, and episode index. Runtime wires these operations into
// the UI and session manager but does not implement the domain logic itself.
package memoryops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/embedder"
	gitw "github.com/VrncQuentin/harness/internal/git"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/retrieval"
	"github.com/VrncQuentin/harness/internal/session"
	"github.com/VrncQuentin/harness/internal/vector"
)

// AfterSaveEmbed returns an AfterSaveFunc that embeds the saved episode and
// updates the project's _episodes index. It indexes the rendered episode body
// (the exact bytes written to disk), so the content hash and chunk boundaries
// match what an on-disk rebuild computes and an unchanged episode is never
// re-embedded.
func AfterSaveEmbed(embedClient embedder.Client, episodeIndex *EpisodeIndex, repo *gitw.Repo) session.AfterSaveFunc {
	return func(ctx context.Context, result session.SaveResult) error {
		if embedClient == nil || episodeIndex == nil {
			return nil
		}
		// Fall back to the summary for callers that predate EpisodeBody; the
		// live Manager always populates the rendered body.
		indexed := result.EpisodeBody
		if strings.TrimSpace(indexed) == "" {
			indexed = result.Summary
		}
		chunks := chunkSummary(indexed)
		if len(chunks) == 0 {
			return nil
		}

		vectors, err := embedClient.Embed(ctx, chunks)
		if err != nil {
			return fmt.Errorf("embed episode %s: %w", result.ID, err)
		}
		if err := episodeIndex.Upsert(retrieval.EpisodeID(result.EpisodePath), contentHash(indexed), vectors); err != nil {
			return fmt.Errorf("index episode %s: %w", result.EpisodePath, err)
		}

		if repo != nil {
			msg := gitw.BuildMessage(
				map[string]string{"type": "index", "episode_id": result.ID},
				"update episode index",
			)
			if _, err := repo.Commit(msg, EpisodeIndexCommitPaths()); err != nil {
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

// RetrievalScore is retrieval metadata for one episode.
type RetrievalScore struct {
	Indexed  bool
	Score    float64
	HasScore bool
}

// EpisodeScorer scores episode paths against an episode index. It opens
// lazily so callers can show scores immediately after a fresh-clone rebuild
// creates the index files.
type EpisodeScorer struct {
	Embedder embedder.Client
	Config   config.PromptConfig
	Index    *EpisodeIndex
}

func (s *EpisodeScorer) ScoreEpisodes(ctx context.Context, query string, episodePaths []string) (map[string]RetrievalScore, error) {
	out := make(map[string]RetrievalScore, len(episodePaths))
	for _, p := range episodePaths {
		out[p] = RetrievalScore{}
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
	scores, scored, err := retrieval.ScoreEpisodePaths(
		ctx,
		s.Embedder,
		s.Index,
		query,
		episodePaths,
		s.Config.SemanticWeight,
		s.Config.RecencyWeight,
	)
	if err != nil {
		return out, err
	}
	if !scored {
		return out, nil
	}
	for _, p := range episodePaths {
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
//
// The rebuilder writes through the same EpisodeIndex the retrieval paths read,
// rather than opening a second handle on the same directory. Two handles would
// mean two in-memory manifests over one pair of files, and whichever wrote last
// would silently drop the other's entries.
type EpisodeRebuilder struct {
	Mem      memory.Repo
	Embedder embedder.Client
	Index    *EpisodeIndex
	Repo     *gitw.Repo
}

func (rb *EpisodeRebuilder) Rebuild(ctx context.Context) error {
	if rb.Index == nil {
		return errors.New("index rebuild: episode index is not available")
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
		hash    string
		chunks  []string
	}
	var work []pending
	allChunkCount := 0
	for _, p := range paths {
		body, err := rb.Mem.Read(p)
		if err != nil {
			slog.Warn("index rebuild: skip unreadable episode", "path", p, "err", err)
			continue
		}
		content := string(body)
		hash := contentHash(content)
		id := retrieval.EpisodeID(p)
		if rb.Index.ContainsCurrent(id, hash) {
			continue
		}
		chunks := chunkSummary(content)
		if len(chunks) == 0 {
			continue
		}
		work = append(work, pending{path: p, content: content, hash: hash, chunks: chunks})
		allChunkCount += len(chunks)
	}

	if len(work) == 0 {
		return nil
	}

	allChunks := make([]string, 0, allChunkCount)
	for _, w := range work {
		allChunks = append(allChunks, w.chunks...)
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

	offset := 0
	for _, w := range work {
		n := len(w.chunks)
		if n == 0 {
			continue
		}
		epVecs := vectors[offset : offset+n]
		offset += n
		if err := rb.Index.Upsert(retrieval.EpisodeID(w.path), w.hash, epVecs); err != nil {
			slog.Warn("index rebuild: add episode", "path", w.path, "err", err)
		}
	}

	if rb.Repo != nil {
		msg := gitw.BuildMessage(
			map[string]string{"type": "index-rebuild"},
			fmt.Sprintf("rebuild episode index: %d new episodes", len(work)),
		)
		if _, err := rb.Repo.Commit(msg, EpisodeIndexCommitPaths()); err != nil {
			slog.Warn("index rebuild: commit", "err", err)
		}
	}

	slog.Info("index rebuild complete", "new_episodes", len(work))
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
		sim := vector.CosineSimilarity(candidate, v)
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
