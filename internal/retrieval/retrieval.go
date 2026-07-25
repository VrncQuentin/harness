package retrieval

import (
	"context"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/index"
)

// EpisodeEmbedder embeds query text for episode scoring.
type EpisodeEmbedder interface {
	Embed(ctx context.Context, chunks []string) ([][]float32, error)
}

// EpisodeSearcher searches the project episode index.
type EpisodeSearcher interface {
	Search(query []float32, k int) ([]index.Result, error)
}

// EpisodeID returns the content SHA/id used for an episode path in the index.
func EpisodeID(epPath string) string {
	return strings.TrimSuffix(strings.ReplaceAll(epPath, "\\", "/"), ".md")
}

// BestSemanticScores folds chunk-level index results into one score per
// episode, keeping the best matching chunk for each episode SHA.
func BestSemanticScores(results []index.Result) map[string]float64 {
	scores := make(map[string]float64, len(results))
	for _, r := range results {
		if existing, ok := scores[r.SHA]; !ok || float64(r.Score) > existing {
			scores[r.SHA] = float64(r.Score)
		}
	}
	return scores
}

// BlendEpisodeScores returns blended semantic + recency scores keyed by path.
// episodePaths must be oldest-first so the newest episode receives the highest
// recency score.
func BlendEpisodeScores(episodePaths []string, semantic map[string]float64, semanticWeight, recencyWeight float64) map[string]float64 {
	out := make(map[string]float64, len(episodePaths))
	n := float64(len(episodePaths))
	if n == 0 {
		return out
	}
	for i, p := range episodePaths {
		id := EpisodeID(p)
		semanticScore, ok := semantic[id]
		if !ok {
			// Read legacy basename-only manifests during the index identity
			// migration. Newly written entries always use the full source path.
			semanticScore = semantic[path.Base(id)]
		}
		out[p] = semanticWeight*semanticScore +
			recencyWeight*Decay(len(episodePaths)-1-i, n)
	}
	return out
}

// ScoreEpisodePaths embeds query, searches the episode index, and blends
// semantic scores with recency. Scores are keyed by the original episode path.
// The boolean reports whether scoring was applied; false means inputs were not
// sufficient for semantic retrieval or the index returned no results.
func ScoreEpisodePaths(ctx context.Context, embedder EpisodeEmbedder, searcher EpisodeSearcher, query string, episodePaths []string, semanticWeight, recencyWeight float64) (map[string]float64, bool, error) {
	if strings.TrimSpace(query) == "" || embedder == nil || searcher == nil || len(episodePaths) == 0 {
		return map[string]float64{}, false, nil
	}
	vecs, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, false, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return map[string]float64{}, false, nil
	}
	results, err := searcher.Search(vecs[0], len(episodePaths)*2)
	if err != nil {
		return nil, false, err
	}
	if len(results) == 0 {
		return map[string]float64{}, false, nil
	}
	oldestFirst := append([]string(nil), episodePaths...)
	sort.Strings(oldestFirst)
	semanticScores := BestSemanticScores(results)
	blended := BlendEpisodeScores(oldestFirst, semanticScores, semanticWeight, recencyWeight)

	if DefaultTraceSink != nil {
		qid := QueryID(query)
		n := float64(len(oldestFirst))
		now := time.Now()
		for i, p := range oldestFirst {
			id := EpisodeID(p)
			sem := semanticScores[id]
			if sem == 0 {
				sem = semanticScores[path.Base(id)]
			}
			DefaultTraceSink.Emit(RetrievalTrace{
				QueryID:       qid,
				EpisodePath:   p,
				SemanticScore: sem,
				RecencyScore:  Decay(len(oldestFirst)-1-i, n),
				BlendedScore:  blended[p],
				Rank:          i,
				Ts:            now,
			})
		}
	}

	return blended, true, nil
}

// Decay returns an exponential recency score where distanceFromNewest=0
// (most recent) gives 1.0 and older episodes decay toward zero. n is the
// total number of episodes (> 0) used to scale the decay rate.
func Decay(distanceFromNewest int, n float64) float64 {
	return math.Exp(-float64(distanceFromNewest) / n)
}
