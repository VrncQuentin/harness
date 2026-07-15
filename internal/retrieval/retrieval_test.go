package retrieval

import (
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
	if got := EpisodeID("projects/global/episodes/coder/abc123.md"); got != "abc123" {
		t.Fatalf("EpisodeID = %q, want abc123", got)
	}
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 0.0000001
}
