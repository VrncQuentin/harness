package retrieval

import (
	"context"
	"math"
	"testing"

	"github.com/vrnc/harness/internal/index"
)

func TestBestSemanticScoresKeepsBestChunk(t *testing.T) {
	got := BestSemanticScores([]index.Result{
		{SHA: "a", Score: 0.2},
		{SHA: "a", Score: 0.8},
		{SHA: "b", Score: -0.1},
	})
	if !nearly(got["a"], 0.8) {
		t.Fatalf("score a = %v, want 0.8", got["a"])
	}
	if !nearly(got["b"], -0.1) {
		t.Fatalf("score b = %v, want -0.1", got["b"])
	}
}

func TestBlendEpisodeScoresUsesOldestFirstRecency(t *testing.T) {
	paths := []string{"episodes/coder/01.md", "episodes/coder/02.md", "episodes/coder/03.md"}
	got := BlendEpisodeScores(paths, map[string]float64{}, 0, 1)

	if got[paths[2]] != 1 {
		t.Fatalf("newest score = %v, want 1", got[paths[2]])
	}
	if !nearly(got[paths[1]], math.Exp(-1.0/3.0)) {
		t.Fatalf("middle score = %v, want exp(-1/3)", got[paths[1]])
	}
	if got[paths[0]] >= got[paths[1]] {
		t.Fatalf("oldest score %v should be lower than middle %v", got[paths[0]], got[paths[1]])
	}
}

func TestEpisodeID(t *testing.T) {
	if got := EpisodeID("projects/global/episodes/coder/abc123.md"); got != "projects/global/episodes/coder/abc123" {
		t.Fatalf("EpisodeID = %q, want projects/global/episodes/coder/abc123", got)
	}
}

func TestScoreEpisodePathsEmbedsSearchesAndBlends(t *testing.T) {
	embedder := scoreEmbedder{vec: []float32{1, 0}}
	searcher := scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/01", Score: 0.2},
		{SHA: "episodes/coder/02", Score: 0.9},
	}}
	paths := []string{"episodes/coder/02.md", "episodes/coder/01.md"}

	got, scored, err := ScoreEpisodePaths(context.Background(), embedder, &searcher, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if !scored {
		t.Fatal("ScoreEpisodePaths scored = false")
	}
	if !nearly(got["episodes/coder/02.md"], 0.9) {
		t.Fatalf("score for 02 = %v, want semantic 0.9", got["episodes/coder/02.md"])
	}
	if !nearly(got["episodes/coder/01.md"], 0.2) {
		t.Fatalf("score for 01 = %v, want semantic 0.2", got["episodes/coder/01.md"])
	}
	if len(searcher.query) != 2 || searcher.query[0] != 1 || searcher.k != 4 {
		t.Fatalf("searcher got query=%v k=%d, want [1 0] k=4", searcher.query, searcher.k)
	}
}

func TestScoreEpisodePathsReturnsUnscoredWithoutQuery(t *testing.T) {
	got, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, &scoreSearcher{}, " ", []string{"episodes/coder/01.md"}, 1, 1)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if scored {
		t.Fatal("ScoreEpisodePaths scored = true, want false")
	}
	if len(got) != 0 {
		t.Fatalf("scores = %v, want empty", got)
	}
}

type scoreEmbedder struct {
	vec []float32
}

func (s scoreEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{s.vec}, nil
}

type scoreSearcher struct {
	query   []float32
	k       int
	results []index.Result
}

func (s *scoreSearcher) Search(query []float32, k int) ([]index.Result, error) {
	s.query = append([]float32(nil), query...)
	s.k = k
	return s.results, nil
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 0.0000001
}
