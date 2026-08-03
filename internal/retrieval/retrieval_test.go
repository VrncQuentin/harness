package retrieval

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/VrncQuentin/harness/internal/index"
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

func TestBlendEpisodeScoresPrefersFullPathOverLegacyBasename(t *testing.T) {
	paths := []string{"episodes/coder/shared.md", "episodes/reviewer/shared.md"}
	semantic := map[string]float64{
		"episodes/coder/shared":    0.2,
		"episodes/reviewer/shared": 0.9,
		"shared":                   0.5,
	}

	got := BlendEpisodeScores(paths, semantic, 1, 0)
	if !nearly(got["episodes/coder/shared.md"], 0.2) {
		t.Fatalf("coder shared score = %v, want full-path score 0.2", got["episodes/coder/shared.md"])
	}
	if !nearly(got["episodes/reviewer/shared.md"], 0.9) {
		t.Fatalf("reviewer shared score = %v, want full-path score 0.9", got["episodes/reviewer/shared.md"])
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

	got, scored, err := ScoreEpisodePaths(context.Background(), embedder, &searcher, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 0)
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
	got, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, &scoreSearcher{}, TraceContext{}, " ", []string{"episodes/coder/01.md"}, 1, 1)
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

// recordingSink captures every emitted trace row for discriminator tests.
type recordingSink struct {
	rows []RetrievalTrace
}

func (s *recordingSink) Emit(t RetrievalTrace) error {
	s.rows = append(s.rows, t)
	return nil
}
func (s *recordingSink) Close() error { return nil }

// withTraceSink installs a recording sink as the package default for the test
// and restores the previous sink afterwards.
func withTraceSink(t *testing.T) *recordingSink {
	t.Helper()
	prev := DefaultTraceSink
	rec := &recordingSink{}
	SetDefaultTraceSink(rec)
	t.Cleanup(func() { SetDefaultTraceSink(prev) })
	return rec
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 0.0000001
}

// TestScoreEpisodePathsEmitsCallRowForBlankQuery: a blank query is unscoreable
// and must still emit exactly one call row with no candidate rows.
func TestScoreEpisodePathsEmitsCallRowForBlankQuery(t *testing.T) {
	rec := withTraceSink(t)
	_, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, &scoreSearcher{}, TraceContext{ProjectSlug: "global"}, "  ", []string{"episodes/coder/01.md"}, 1, 1)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if scored {
		t.Fatal("blank query must be unscored")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("want exactly one call row, got %d", len(rec.rows))
	}
	row := rec.rows[0]
	if row.RecordType != RecordTypeCall {
		t.Errorf("RecordType: want call, got %s", row.RecordType)
	}
	if row.Outcome != OutcomeUnscoreable {
		t.Errorf("Outcome: want unscoreable, got %s", row.Outcome)
	}
	if row.ProjectSlug != "global" {
		t.Errorf("ProjectSlug: want global, got %s", row.ProjectSlug)
	}
	if row.Version != TraceSchemaVersion {
		t.Errorf("Version: want %d, got %d", TraceSchemaVersion, row.Version)
	}
	if len(row.QueryID) != 64 {
		t.Errorf("QueryID: want full SHA-256, got %d chars", len(row.QueryID))
	}
}

// TestScoreEpisodePathsEmitsCallRowForUnavailableDeps: a nil embedder or nil
// searcher is unavailable and must emit exactly one unscoreable call row.
func TestScoreEpisodePathsEmitsCallRowForUnavailableDeps(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/01.md"}
	_, scored, err := ScoreEpisodePaths(context.Background(), nil, &scoreSearcher{}, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 1)
	if err != nil {
		t.Fatalf("nil embedder: %v", err)
	}
	if scored {
		t.Fatal("nil embedder must be unscored")
	}
	if len(rec.rows) != 1 || rec.rows[0].Outcome != OutcomeUnscoreable {
		t.Fatalf("nil embedder: want one unscoreable call row, got %+v", rec.rows)
	}

	rec = withTraceSink(t)
	_, scored, err = ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, nil, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 1)
	if err != nil {
		t.Fatalf("nil searcher: %v", err)
	}
	if scored {
		t.Fatal("nil searcher must be unscored")
	}
	if len(rec.rows) != 1 || rec.rows[0].Outcome != OutcomeUnscoreable {
		t.Fatalf("nil searcher: want one unscoreable call row, got %+v", rec.rows)
	}
}

// TestScoreEpisodePathsEmitsCallRowForEmptyResults: an index that returns no
// results is unscoreable and must emit exactly one call row, not silently
// disappear.
func TestScoreEpisodePathsEmitsCallRowForEmptyResults(t *testing.T) {
	rec := withTraceSink(t)
	_, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, &scoreSearcher{results: nil}, TraceContext{ProjectSlug: "global"}, "needle", []string{"episodes/coder/01.md"}, 1, 1)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if scored {
		t.Fatal("empty results must be unscored")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("want exactly one call row, got %d", len(rec.rows))
	}
	if rec.rows[0].Outcome != OutcomeUnscoreable {
		t.Errorf("Outcome: want unscoreable, got %s", rec.rows[0].Outcome)
	}
}

// TestScoreEpisodePathsEmitsCallRowForError: a scoring error must emit exactly
// one call row with the error outcome and propagate the error.
func TestScoreEpisodePathsEmitsCallRowForError(t *testing.T) {
	rec := withTraceSink(t)
	_, _, err := ScoreEpisodePaths(context.Background(), errEmbedder{}, &scoreSearcher{}, TraceContext{ProjectSlug: "global"}, "needle", []string{"episodes/coder/01.md"}, 1, 1)
	if err == nil {
		t.Fatal("embed error must propagate")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("want exactly one call row, got %d", len(rec.rows))
	}
	if rec.rows[0].Outcome != OutcomeError {
		t.Errorf("Outcome: want error, got %s", rec.rows[0].Outcome)
	}
}

type errEmbedder struct{}

func (errEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedder offline")
}

// TestScoreEpisodePathsEmitsCandidateRowsWithFinalRank: a scored invocation
// emits one call row plus one candidate row per episode, ranked by final
// blended score descending (one-based), not by path order.
func TestScoreEpisodePathsEmitsCandidateRowsWithFinalRank(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{
		"episodes/coder/oldest.md", // semantic 0.2 -> lowest
		"episodes/coder/mid.md",    // semantic 0.5
		"episodes/coder/newest.md", // semantic 0.9 -> highest
	}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/oldest", Score: 0.2},
		{SHA: "episodes/coder/mid", Score: 0.5},
		{SHA: "episodes/coder/newest", Score: 0.9},
	}}
	scores, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if !scored {
		t.Fatal("scored = false, want true")
	}
	if len(scores) != 3 {
		t.Fatalf("scores: want 3, got %d", len(scores))
	}

	var calls, candidates []RetrievalTrace
	for _, r := range rec.rows {
		switch r.RecordType {
		case RecordTypeCall:
			calls = append(calls, r)
		case RecordTypeCandidate:
			candidates = append(candidates, r)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("want exactly one call row, got %d", len(calls))
	}
	if calls[0].Outcome != OutcomeScored {
		t.Errorf("call Outcome: want scored, got %s", calls[0].Outcome)
	}
	if len(candidates) != 3 {
		t.Fatalf("want 3 candidate rows, got %d", len(candidates))
	}

	// Rank order must be newest (0.9), mid (0.5), oldest (0.2), even though
	// the input order was oldest-first.
	wantOrder := []string{
		"episodes/coder/newest.md",
		"episodes/coder/mid.md",
		"episodes/coder/oldest.md",
	}
	for i, r := range candidates {
		if r.Candidate != wantOrder[i] {
			t.Errorf("candidate %d: want %s, got %s", i, wantOrder[i], r.Candidate)
		}
		if r.Rank != i+1 {
			t.Errorf("candidate %d rank: want %d (one-based), got %d", i, i+1, r.Rank)
		}
		if r.RecordType != RecordTypeCandidate {
			t.Errorf("candidate %d RecordType: want candidate, got %s", i, r.RecordType)
		}
		if r.ProjectSlug != "global" {
			t.Errorf("candidate %d ProjectSlug: want global, got %s", i, r.ProjectSlug)
		}
		if r.Version != TraceSchemaVersion {
			t.Errorf("candidate %d Version: want %d, got %d", i, TraceSchemaVersion, r.Version)
		}
		if len(r.QueryID) != 64 {
			t.Errorf("candidate %d QueryID: want full SHA-256, got %d chars", i, len(r.QueryID))
		}
	}
	if !nearly(candidates[0].Semantic, 0.9) || !nearly(candidates[0].Score, 0.9) {
		t.Errorf("top candidate semantic/score: got %f/%f, want 0.9/0.9", candidates[0].Semantic, candidates[0].Score)
	}
}

// TestScoreEpisodePathsRecordsWeights: candidate rows must record the
// configured weights and the final blended score, and semantic-only blending
// with a recency weight must still expose the raw recency component.
func TestScoreEpisodePathsRecordsWeights(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/01.md", "episodes/coder/02.md"}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/02", Score: 1.0},
	}}
	_, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global", TopK: 1}, "needle", paths, 0.5, 0.25)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if !scored {
		t.Fatal("scored = false, want true")
	}
	var candidates []RetrievalTrace
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCandidate {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidate rows, got %d", len(candidates))
	}
	for _, r := range candidates {
		if r.SemanticWeight != 0.5 || r.RecencyWeight != 0.25 {
			t.Errorf("weights: got %f/%f, want 0.5/0.25", r.SemanticWeight, r.RecencyWeight)
		}
		if r.Score != 0.5*r.Semantic+0.25*r.Recency {
			t.Errorf("blended score %f does not match weights %f/%f with sem %f rec %f", r.Score, r.SemanticWeight, r.RecencyWeight, r.Semantic, r.Recency)
		}
	}
}

// TestScoreEpisodePathsReturnedFollowsTopK: Returned must be true only for
// candidates within the requested top-K.
func TestScoreEpisodePathsReturnedFollowsTopK(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/a.md", "episodes/coder/b.md", "episodes/coder/c.md"}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/a", Score: 0.1},
		{SHA: "episodes/coder/b", Score: 0.5},
		{SHA: "episodes/coder/c", Score: 0.9},
	}}
	_, _, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global", TopK: 2}, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	var candidates []RetrievalTrace
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCandidate {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) != 3 {
		t.Fatalf("want 3 candidate rows, got %d", len(candidates))
	}
	// Rank order: c (0.9, returned), b (0.5, returned), a (0.1, not returned).
	if !candidates[0].Returned || !candidates[1].Returned {
		t.Errorf("top-2 candidates must be Returned: got %+v", candidates)
	}
	if candidates[2].Returned {
		t.Errorf("rank-3 candidate must not be Returned: got %+v", candidates[2])
	}
}

// TestScoreEpisodePathsReturnedUnlimitedTopK: TopK <= 0 means unlimited, so
// every scored candidate is returned.
func TestScoreEpisodePathsReturnedUnlimitedTopK(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/a.md", "episodes/coder/b.md"}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/a", Score: 0.1},
		{SHA: "episodes/coder/b", Score: 0.9},
	}}
	_, _, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global", TopK: 0}, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCandidate && !r.Returned {
			t.Errorf("unlimited top-K must return every candidate: %+v", r)
		}
	}
}

// TestScoreEpisodePathsMultiProjectNoCollision: identical relative episode
// paths in two projects must emit rows carrying each project's slug, so the
// same path never collides across projects.
func TestScoreEpisodePathsMultiProjectNoCollision(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/shared.md"}
	searcher := &scoreSearcher{results: []index.Result{{SHA: "episodes/coder/shared", Score: 0.9}}}
	emb := scoreEmbedder{vec: []float32{1}}

	if _, _, err := ScoreEpisodePaths(context.Background(), emb, searcher, TraceContext{ProjectSlug: "alpha"}, "needle", paths, 1, 0); err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if _, _, err := ScoreEpisodePaths(context.Background(), emb, searcher, TraceContext{ProjectSlug: "beta"}, "needle", paths, 1, 0); err != nil {
		t.Fatalf("beta: %v", err)
	}

	var got []string
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCandidate {
			got = append(got, r.ProjectSlug+"|"+r.Candidate)
		}
	}
	if !slices.Equal(got, []string{"alpha|episodes/coder/shared.md", "beta|episodes/coder/shared.md"}) {
		t.Errorf("project-scoped rows = %v, want alpha then beta rows", got)
	}
}

// TestScoreEpisodePathsNoTraceWithoutProject: a zero-value TraceContext (no
// project slug) emits nothing even with a sink installed.
func TestScoreEpisodePathsNoTraceWithoutProject(t *testing.T) {
	rec := withTraceSink(t)
	searcher := &scoreSearcher{results: []index.Result{{SHA: "episodes/coder/a", Score: 0.9}}}
	_, _, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{}, "needle", []string{"episodes/coder/a.md"}, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("zero-value TraceContext must not emit, got %d rows", len(rec.rows))
	}
}

// TestScoreEpisodePathsInvocationIDAssociatesCallAndCandidates: one fresh
// invocation ID must be shared by the call row and every candidate row of the
// same invocation, so candidates are associable with their call even when the
// same query repeats or concurrent emissions interleave.
func TestScoreEpisodePathsInvocationIDAssociatesCallAndCandidates(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/a.md", "episodes/coder/b.md"}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/a", Score: 0.9},
		{SHA: "episodes/coder/b", Score: 0.1},
	}}
	_, _, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}

	var callID string
	var candidateIDs []string
	for _, r := range rec.rows {
		if r.InvocationID == "" {
			t.Error("row has an empty invocation_id")
		}
		switch r.RecordType {
		case RecordTypeCall:
			callID = r.InvocationID
		case RecordTypeCandidate:
			candidateIDs = append(candidateIDs, r.InvocationID)
		}
	}
	if callID == "" || len(candidateIDs) != 2 {
		t.Fatalf("call id %q candidates %v", callID, candidateIDs)
	}
	for _, cid := range candidateIDs {
		if cid != callID {
			t.Errorf("candidate invocation_id %q differs from call %q", cid, callID)
		}
	}
}

// TestScoreEpisodePathsInvocationIDDistinctPerCall: two separate invocations
// of the same query must mint distinct invocation IDs, even though the query
// hash is identical.
func TestScoreEpisodePathsInvocationIDDistinctPerCall(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/a.md"}
	searcher := &scoreSearcher{results: []index.Result{{SHA: "episodes/coder/a", Score: 0.9}}}
	emb := scoreEmbedder{vec: []float32{1}}
	for range 2 {
		if _, _, err := ScoreEpisodePaths(context.Background(), emb, searcher, TraceContext{ProjectSlug: "global"}, "same query", paths, 1, 0); err != nil {
			t.Fatalf("ScoreEpisodePaths: %v", err)
		}
	}
	var callRows []RetrievalTrace
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCall {
			callRows = append(callRows, r)
		}
	}
	if len(callRows) != 2 {
		t.Fatalf("want 2 call rows, got %d", len(callRows))
	}
	if callRows[0].InvocationID == "" || callRows[0].InvocationID == callRows[1].InvocationID {
		t.Fatalf("distinct invocations must mint distinct ids: %q vs %q", callRows[0].InvocationID, callRows[1].InvocationID)
	}
	// The query hash is the same, which is exactly why the invocation ID is
	// needed to disambiguate the two calls.
	if callRows[0].QueryID != callRows[1].QueryID {
		t.Fatalf("expected identical QueryID across identical queries: %q vs %q", callRows[0].QueryID, callRows[1].QueryID)
	}
}

// TestScoreEpisodePathsSemanticFallbackCommaOK: a legitimate full-path zero
// semantic score must not fall back to a legacy basename entry; the recorded
// Semantic must match the zero actually used to compute the blend. The search
// results carry a basename "shared" entry with 0.5 alongside the full-path
// zero, so reverting the comma-ok lookup to a value-zero check would record
// 0.5 here and fail.
func TestScoreEpisodePathsSemanticFallbackCommaOK(t *testing.T) {
	rec := withTraceSink(t)
	paths := []string{"episodes/coder/shared.md"}
	searcher := &scoreSearcher{results: []index.Result{
		{SHA: "episodes/coder/shared", Score: 0},
		{SHA: "shared", Score: 0.5},
	}}
	// BlendEpisodeScores uses comma-ok: the full-path key is present with a
	// legitimate zero, so the blend used 0*1 + 0*recency = 0 and the trace
	// must record Semantic 0, not the basename 0.5.
	_, scored, err := ScoreEpisodePaths(context.Background(), scoreEmbedder{vec: []float32{1}}, searcher, TraceContext{ProjectSlug: "global"}, "needle", paths, 1, 0)
	if err != nil {
		t.Fatalf("ScoreEpisodePaths: %v", err)
	}
	if !scored {
		t.Fatal("scored = false, want true")
	}
	var candidate RetrievalTrace
	for _, r := range rec.rows {
		if r.RecordType == RecordTypeCandidate {
			candidate = r
		}
	}
	if candidate.Candidate == "" {
		t.Fatal("no candidate row emitted")
	}
	if !nearly(candidate.Semantic, 0) {
		t.Errorf("Semantic = %v, want 0 (a present full-path zero must not fall back to a basename entry)", candidate.Semantic)
	}
	if !nearly(candidate.Score, 0) {
		t.Errorf("Score = %v, want 0", candidate.Score)
	}
}
