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
//
// Emission: when tracing is enabled (a sink is installed and tc carries a
// project slug), every invocation emits exactly one call row, including blank,
// unavailable, unscoreable, and failed outcomes, and a scored invocation emits
// one candidate row per scored episode with its final post-sort rank, the
// configured weights, and whether it falls within the caller's requested top-K.
func ScoreEpisodePaths(ctx context.Context, embedder EpisodeEmbedder, searcher EpisodeSearcher, tc TraceContext, query string, episodePaths []string, semanticWeight, recencyWeight float64) (map[string]float64, bool, error) {
	if strings.TrimSpace(query) == "" || embedder == nil || searcher == nil || len(episodePaths) == 0 {
		emitCall(tc, query, semanticWeight, recencyWeight, OutcomeUnscoreable)
		return map[string]float64{}, false, nil
	}
	vecs, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		emitCall(tc, query, semanticWeight, recencyWeight, OutcomeError)
		return nil, false, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		emitCall(tc, query, semanticWeight, recencyWeight, OutcomeUnscoreable)
		return map[string]float64{}, false, nil
	}
	results, err := searcher.Search(vecs[0], len(episodePaths)*2)
	if err != nil {
		emitCall(tc, query, semanticWeight, recencyWeight, OutcomeError)
		return nil, false, err
	}
	if len(results) == 0 {
		emitCall(tc, query, semanticWeight, recencyWeight, OutcomeUnscoreable)
		return map[string]float64{}, false, nil
	}
	oldestFirst := append([]string(nil), episodePaths...)
	sort.Strings(oldestFirst)
	semanticScores := BestSemanticScores(results)
	blended := BlendEpisodeScores(oldestFirst, semanticScores, semanticWeight, recencyWeight)

	emitCall(tc, query, semanticWeight, recencyWeight, OutcomeScored)
	emitCandidates(tc, query, oldestFirst, semanticScores, blended, semanticWeight, recencyWeight)
	return blended, true, nil
}

// emitCall writes the one call row for an invocation when tracing is enabled.
// The outcome names the invocation's result: scored (retrieval ran, possibly
// finding nothing), unscoreable (inputs could not produce scores), or error.
func emitCall(tc TraceContext, query string, semanticWeight, recencyWeight float64, outcome string) {
	emitRow(tc, RetrievalTrace{
		Version:        TraceSchemaVersion,
		RecordType:     RecordTypeCall,
		ProjectSlug:    tc.ProjectSlug,
		QueryID:        QueryID(query),
		SemanticWeight: semanticWeight,
		RecencyWeight:  recencyWeight,
		Outcome:        outcome,
		Timestamp:      time.Now(),
	})
}

// emitCandidates writes one candidate row per scored episode, in final
// post-sort rank order (one-based rank by blended score descending). Recency
// is the raw exp_decay value for the episode's age, independent of rank.
func emitCandidates(tc TraceContext, query string, oldestFirst []string, semantic map[string]float64, blended map[string]float64, semanticWeight, recencyWeight float64) {
	n := float64(len(oldestFirst))
	recency := make(map[string]float64, len(oldestFirst))
	for i, p := range oldestFirst {
		recency[p] = Decay(len(oldestFirst)-1-i, n)
	}
	ranked := append([]string(nil), oldestFirst...)
	sort.SliceStable(ranked, func(i, j int) bool { return blended[ranked[i]] > blended[ranked[j]] })
	qid := QueryID(query)
	now := time.Now()
	for i, p := range ranked {
		id := EpisodeID(p)
		sem := semantic[id]
		if sem == 0 {
			sem = semantic[path.Base(id)]
		}
		rank := i + 1
		emitRow(tc, RetrievalTrace{
			Version:        TraceSchemaVersion,
			RecordType:     RecordTypeCandidate,
			ProjectSlug:    tc.ProjectSlug,
			QueryID:        qid,
			Candidate:      p,
			Semantic:       sem,
			Recency:        recency[p],
			SemanticWeight: semanticWeight,
			RecencyWeight:  recencyWeight,
			Score:          blended[p],
			Rank:           rank,
			Returned:       tc.TopK <= 0 || rank <= tc.TopK,
			Timestamp:      now,
		})
	}
}

// emitRow writes one trace row when tracing is enabled. A row is emitted only
// when a sink is installed and the invocation carries a project slug, so
// display-only callers that pass a zero-value TraceContext emit nothing.
func emitRow(tc TraceContext, row RetrievalTrace) {
	if DefaultTraceSink == nil || tc.ProjectSlug == "" {
		return
	}
	DefaultTraceSink.Emit(row)
}

// Decay returns an exponential recency score where distanceFromNewest=0
// (most recent) gives 1.0 and older episodes decay toward zero. n is the
// total number of episodes (> 0) used to scale the decay rate.
func Decay(distanceFromNewest int, n float64) float64 {
	return math.Exp(-float64(distanceFromNewest) / n)
}
