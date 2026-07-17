package retrieval

import (
	"math"
	"path"
	"strings"

	"github.com/vrnc/harness/internal/index"
)

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

// Decay returns an exponential recency score where distanceFromNewest=0
// (most recent) gives 1.0 and older episodes decay toward zero. n is the
// total number of episodes (> 0) used to scale the decay rate.
func Decay(distanceFromNewest int, n float64) float64 {
	return math.Exp(-float64(distanceFromNewest) / n)
}
