package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/index"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkDir creates a directory link at link pointing at target, preferring a
// symlink and falling back to a Windows junction. Junctions need no privilege
// and are traversed exactly like symlinks, so they exercise the same escape on
// machines where symlink creation is denied.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory link: %v: %s", err, out)
	}
}

// stubEmbedder returns a fixed vector so the production scoring path can run
// without a sidecar. The episode index is empty in these tests, so semantic
// retrieval contributes nothing; the discrimination is about enumeration.
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	vecs := make([][]float32, len(chunks))
	for i := range vecs {
		vecs[i] = []float32{0.1, 0.2}
	}
	return vecs, nil
}

// eval-retrieval must enumerate episodes through a pinned repo reader,
// producing stable repo-relative forward-slash paths, rather than
// filepath.Glob + filepath.Rel on an operator-supplied root. The test drives
// evaluate — the exact function run() executes — and asserts the paths it
// returns, so reverting enumeration to pathname globs changes the result.
func TestEvalRetrieval_PinnedRepo(t *testing.T) {
	t.Run("pinned enumeration", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
		writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-02.md"), "two")
		writeFile(t, filepath.Join(root, "episodes", "architect", "2024-01-03.md"), "three")
		// .md files outside the depth-two episode shape must not be enumerated.
		writeFile(t, filepath.Join(root, "rules.md"), "not an episode")
		writeFile(t, filepath.Join(root, "episodes", "top.md"), "not an episode")

		queries := []queryRecord{{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}}
		paths, err := evaluate(root, queries, stubEmbedder{}, evalOptions{K: 3})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		want := []string{
			"episodes/architect/2024-01-03.md",
			"episodes/coder/2024-01-01.md",
			"episodes/coder/2024-01-02.md",
		}
		if !slices.Equal(paths, want) {
			t.Errorf("evaluated paths = %v, want %v", paths, want)
		}
	})

	// The escape the old mechanism would follow: a directory link at
	// episodes/linked pointing at a tree with a directly-matching .md. The old
	// episodes/*/*.md glob would list <repo>/episodes/linked/*.md and match the
	// outside file through the link; the pinned walk never follows the link, so
	// the outside path is not among the enumerated paths. Whatever the walk
	// outcome (a refusal is also safe), the outside path must never appear.
	t.Run("escaping link excluded", func(t *testing.T) {
		base := t.TempDir()
		writeFile(t, filepath.Join(base, "outside", "episodes", "leak.md"), "SECRET")
		writeFile(t, filepath.Join(base, "repo", "episodes", "coder", "2024-01-01.md"), "real")
		linkDir(t, filepath.Join(base, "outside", "episodes"), filepath.Join(base, "repo", "episodes", "linked"))

		queries := []queryRecord{{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}}
		paths, err := evaluate(filepath.Join(base, "repo"), queries, stubEmbedder{}, evalOptions{K: 3})
		for _, p := range paths {
			if p == "episodes/linked/leak.md" {
				t.Fatalf("outside file was enumerated through the link: %v", paths)
			}
		}
		if err != nil {
			return // a refusal is the fail-closed outcome
		}
		if !slices.Contains(paths, "episodes/coder/2024-01-01.md") {
			t.Errorf("real episode missing from enumeration: %v", paths)
		}
	})
}

// loadQueries must reject a row whose labeled-query schema version the
// evaluator does not recognize, naming the row and the offending version.
func TestLoadQueries_RejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "queries.ndjson")
	writeFile(t, p, "{\"version\":2,\"query\":\"future schema\",\"relevant\":[\"episodes/coder/1.md\"]}\n")

	_, err := loadQueries(p)
	if err == nil {
		t.Fatal("loadQueries accepted an unsupported schema version")
	}
	if !strings.Contains(err.Error(), "unsupported labeled-query schema version 2") {
		t.Errorf("error should name the unsupported version: %v", err)
	}
}

// loadQueries must reject a row with a missing version field: the schema is
// versioned, so an unversioned row is not a valid labeled query.
func TestLoadQueries_RejectsMissingVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "queries.ndjson")
	writeFile(t, p, "{\"query\":\"unversioned\",\"relevant\":[\"episodes/coder/1.md\"]}\n")

	_, err := loadQueries(p)
	if err == nil {
		t.Fatal("loadQueries accepted an unversioned row")
	}
	if !strings.Contains(err.Error(), "schema version 0") {
		t.Errorf("error should name the missing (0) version: %v", err)
	}
}

// loadQueries must reject a malformed row and report its line number.
func TestLoadQueries_RejectsMalformedRow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "queries.ndjson")
	writeFile(t, p, "{\"version\":1,\"query\":\"ok\",\"relevant\":[]}\nnot json\n")

	_, err := loadQueries(p)
	if err == nil {
		t.Fatal("loadQueries accepted a malformed row")
	}
	if !strings.Contains(err.Error(), "row 2") {
		t.Errorf("error should name the malformed row: %v", err)
	}
}

// loadQueries must reject an empty file: an evaluation over zero queries
// measures nothing.
func TestLoadQueries_RejectsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "queries.ndjson")
	writeFile(t, p, "")

	_, err := loadQueries(p)
	if err == nil {
		t.Fatal("loadQueries accepted an empty file")
	}
}

// testSearcher returns scripted semantic results keyed by episode source SHA.
type testSearcher struct {
	results []index.Result
	err     error
}

func (s *testSearcher) Search(_ []float32, _ int) ([]index.Result, error) {
	return s.results, s.err
}

// errSearcher always fails, used to prove baseline mode surfaces scoring errors.
type errSearcher struct{ err error }

func (s *errSearcher) Search(_ []float32, _ int) ([]index.Result, error) { return nil, s.err }

// okSearcher returns no results without error.
type okSearcher struct{}

func (okSearcher) Search(_ []float32, _ int) ([]index.Result, error) { return nil, nil }

// testEmbedder returns a fixed non-empty vector so scoring runs.
type testEmbedder struct{}

func (testEmbedder) Embed(_ context.Context, chunks []string) ([][]float32, error) {
	vecs := make([][]float32, len(chunks))
	for i := range vecs {
		vecs[i] = []float32{1, 0}
	}
	return vecs, nil
}

// evalPaths is the fixed oldest-first episode list used by the metric tests.
// The names are ISO timestamps so lexicographic order is chronological, which
// is exactly the ordering ScoreEpisodePaths and the evaluator rely on for the
// recency signal.
var evalPaths = []string{
	"episodes/coder/2024-01-01.md", // oldest
	"episodes/coder/2024-01-02.md", // middle
	"episodes/coder/2024-01-03.md", // newest
}

// evalOpts is the standard K=3 evaluation options used by the metric tests.
func evalOpts() evalOptions {
	return evalOptions{K: 3, SemanticWeight: 0.5, RecencyWeight: 0.5}
}

// TestMetrics_SemanticOnlyRecencyOnlyAndBlend: the three signals must be
// reported separately over the same labels. Semantic-only is pure similarity;
// recency-only is pure recency; the blend combines both weights. The relevant
// hit that is not the newest episode discriminates the modes.
func TestMetrics_SemanticOnlyRecencyOnlyAndBlend(t *testing.T) {
	searcher := &testSearcher{results: []index.Result{
		{SHA: "episodes/coder/2024-01-03", Score: 0.9}, // newest, matches "needle"
		{SHA: "episodes/coder/2024-01-02", Score: 0.5},
		{SHA: "episodes/coder/2024-01-01", Score: 0.1},
	}}

	// newest.md (2024-01-03) is relevant and is both the semantic and recency
	// winner, so every mode ranks it first.
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "needle", Relevant: []string{"episodes/coder/2024-01-03.md"}}
	report, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, evalPaths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	for mode, m := range map[string]modeMetrics{"semantic": report.Semantic, "recency": report.Recency, "blend": report.Blend} {
		if !nearly(m.Precision, 1.0/3.0) {
			t.Errorf("%s Precision@3 = %v, want 1/3", mode, m.Precision)
		}
		if !nearly(m.Recall, 1.0) {
			t.Errorf("%s Recall@3 = %v, want 1", mode, m.Recall)
		}
		if !nearly(m.MRR, 1.0) {
			t.Errorf("%s MRR = %v, want 1", mode, m.MRR)
		}
	}

	// The relevant hit is the oldest episode: semantic-only still surfaces it
	// within top-3, recency-only ranks it last, and the blend (0.5, 0.5) falls
	// between the two signals.
	q2 := queryRecord{Version: LabeledQuerySchemaVersion, Query: "old needle", Relevant: []string{"episodes/coder/2024-01-01.md"}}
	report2, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q2, evalPaths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	// Semantic-only: 2024-01-01 has the lowest similarity (0.1), so it is rank
	// 3; P@3 = 1/3, MRR = 1/3.
	if !nearly(report2.Semantic.Precision, 1.0/3.0) || !nearly(report2.Semantic.MRR, 1.0/3.0) {
		t.Errorf("semantic-only for oldest relevant = p%v mrr%v, want 1/3, 1/3", report2.Semantic.Precision, report2.Semantic.MRR)
	}
	// Recency-only: the oldest episode is rank 3 by recency too, so it is
	// still returned; P@3 = 1/3, MRR = 1/3.
	if !nearly(report2.Recency.Precision, 1.0/3.0) || !nearly(report2.Recency.MRR, 1.0/3.0) {
		t.Errorf("recency-only for oldest relevant = p%v mrr%v, want 1/3, 1/3", report2.Recency.Precision, report2.Recency.MRR)
	}
	// Blend (0.5, 0.5): the oldest episode's blend is 0.5*0.1 + 0.5*Decay(2,3),
	// while the newest is 0.5*0.9 + 0.5*1.0; the oldest stays rank 3, returned.
	if !nearly(report2.Blend.Precision, 1.0/3.0) {
		t.Errorf("blend Precision@3 = %v, want 1/3", report2.Blend.Precision)
	}
}

// TestMetrics_FewerThanThreeResults: with fewer than three episodes, Precision@K
// must not divide by zero and must reflect the returned subset, not penalize a
// corpus that cannot produce three results.
func TestMetrics_FewerThanThreeResults(t *testing.T) {
	paths := []string{"episodes/coder/a.md", "episodes/coder/b.md"}
	searcher := &testSearcher{results: []index.Result{
		{SHA: "episodes/coder/a", Score: 0.9},
		{SHA: "episodes/coder/b", Score: 0.1},
	}}
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/a.md"}}
	report, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, paths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	// Only two episodes exist; Precision@3 = 1/2 (the one relevant of two), not 1/3.
	if !nearly(report.Semantic.Precision, 0.5) {
		t.Errorf("semantic Precision@3 with 2 episodes = %v, want 0.5", report.Semantic.Precision)
	}
	if !nearly(report.Semantic.Recall, 1.0) {
		t.Errorf("semantic Recall@3 = %v, want 1", report.Semantic.Recall)
	}
}

// TestMetrics_NoRelevantResults: a query with no relevant episodes must yield
// zero precision and recall without dividing by zero.
func TestMetrics_NoRelevantResults(t *testing.T) {
	searcher := &testSearcher{results: []index.Result{
		{SHA: "episodes/coder/newest", Score: 0.9},
		{SHA: "episodes/coder/mid", Score: 0.5},
	}}
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: nil}
	report, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, evalPaths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	for mode, m := range map[string]modeMetrics{"semantic": report.Semantic, "recency": report.Recency, "blend": report.Blend} {
		if !nearly(m.Precision, 0) {
			t.Errorf("%s Precision@3 with no relevant = %v, want 0", mode, m.Precision)
		}
		if !nearly(m.Recall, 0) {
			t.Errorf("%s Recall@3 with no relevant = %v, want 0", mode, m.Recall)
		}
		if !nearly(m.MRR, 0) {
			t.Errorf("%s MRR with no relevant = %v, want 0", mode, m.MRR)
		}
	}
}

// TestMetrics_Ties: episodes with identical blended scores keep the input
// (oldest-first) order, and the ranking is stable and deterministic.
func TestMetrics_Ties(t *testing.T) {
	searcher := &testSearcher{results: []index.Result{
		{SHA: "episodes/coder/2024-01-01", Score: 0.5},
		{SHA: "episodes/coder/2024-01-02", Score: 0.5},
		{SHA: "episodes/coder/2024-01-03", Score: 0.5},
	}}
	// Relevant is the oldest; with a tie the oldest-first order wins, so the
	// relevant episode is rank 1.
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}
	report, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, evalPaths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	if !nearly(report.Semantic.Precision, 1.0/3.0) {
		t.Errorf("semantic Precision@3 with a tie = %v, want 1/3", report.Semantic.Precision)
	}
	if !nearly(report.Semantic.MRR, 1.0) {
		t.Errorf("semantic MRR with a tie = %v, want 1 (oldest-first tie break)", report.Semantic.MRR)
	}
}

// TestMetrics_MRRUsesCompleteRanking: MRR must examine the whole ranking, not
// only the top-K. A relevant hit at rank 4 yields MRR 1/4 even though Precision
// and Recall are computed at K=3.
func TestMetrics_MRRUsesCompleteRanking(t *testing.T) {
	paths := []string{
		"episodes/coder/2024-01-01.md",
		"episodes/coder/2024-01-02.md",
		"episodes/coder/2024-01-03.md",
		"episodes/coder/2024-01-04.md",
	}
	searcher := &testSearcher{results: []index.Result{
		{SHA: "episodes/coder/2024-01-01", Score: 0.1},
		{SHA: "episodes/coder/2024-01-02", Score: 0.2},
		{SHA: "episodes/coder/2024-01-03", Score: 0.3},
		{SHA: "episodes/coder/2024-01-04", Score: 0.4}, // rank 1
	}}
	// The only relevant episode is the top semantic hit (2024-01-04, rank 1).
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-04.md"}}
	report, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, paths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	if !nearly(report.Semantic.MRR, 1.0) {
		t.Errorf("semantic MRR for top hit = %v, want 1", report.Semantic.MRR)
	}

	// A relevant hit below K must still contribute MRR: relevant is the lowest
	// semantic hit (rank 4), so MRR is 1/4 even though it is not in top-3.
	q2 := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}
	report2, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q2, paths, evalOpts())
	if err != nil {
		t.Fatalf("scoreQuery: %v", err)
	}
	if !nearly(report2.Semantic.MRR, 0.25) {
		t.Errorf("semantic MRR for rank-4 hit = %v, want 0.25 (must inspect the complete ranking, not top-K)", report2.Semantic.MRR)
	}
	if !nearly(report2.Semantic.Precision, 0) {
		t.Errorf("semantic Precision@3 for rank-4 hit = %v, want 0", report2.Semantic.Precision)
	}
}

// TestBaseline_RejectsFewerThanTen: baseline mode must reject fewer than ten
// genuinely evaluated queries rather than producing a baseline from an
// underpowered set. The gate counts evaluations, not input rows.
func TestBaseline_RejectsFewerThanTen(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
	queries := make([]queryRecord, 9)
	for i := range queries {
		queries[i] = queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: nil}
	}
	_, err := evaluate(root, queries, stubEmbedder{}, evalOptions{K: 3, Baseline: true, ResultsDir: t.TempDir()})
	if err == nil {
		t.Fatal("baseline mode accepted fewer than ten evaluated queries")
	}
	if !strings.Contains(err.Error(), "at least 10 genuinely evaluated queries") {
		t.Errorf("baseline error should name the evaluated minimum: %v", err)
	}
}

// TestBaseline_FailsWhenLabelsFailToScore: baseline mode must fail when a
// scoring error or an unscoreable label occurs, rather than silently skipping
// it and still writing a baseline from whatever evaluated.
func TestBaseline_FailsWhenLabelsFailToScore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
	queries := make([]queryRecord, 10)
	for i := range queries {
		queries[i] = queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: nil}
	}
	// A searcher that fails: baseline must return the error, not skip.
	bad := &errSearcher{err: os.ErrPermission}
	_, err := evaluateWithSearcher(root, queries, stubEmbedder{}, bad, evalOptions{K: 3, Baseline: true, ResultsDir: t.TempDir()})
	if err == nil {
		t.Fatal("baseline mode must fail when scoring errors occur")
	}
	if !strings.Contains(err.Error(), os.ErrPermission.Error()) {
		t.Errorf("baseline error should wrap the scoring failure: %v", err)
	}

	// A blank label is unscoreable: baseline must fail rather than count it.
	blank := make([]queryRecord, 10)
	for i := range blank {
		blank[i] = queryRecord{Version: LabeledQuerySchemaVersion, Query: "  ", Relevant: nil}
	}
	_, err = evaluateWithSearcher(root, blank, stubEmbedder{}, &okSearcher{}, evalOptions{K: 3, Baseline: true, ResultsDir: t.TempDir()})
	if err == nil {
		t.Fatal("baseline mode must fail on an unscoreable (blank) label")
	}
	if !strings.Contains(err.Error(), "blank query") {
		t.Errorf("baseline error should name the blank query: %v", err)
	}
}

// TestBaseline_WritesMachineReadableResult: baseline mode writes the aggregate
// metrics as a stable machine-readable document under the results directory.
func TestBaseline_WritesMachineReadableResult(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-01.md"), "one")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-02.md"), "two")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-03.md"), "three")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-04.md"), "four")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-05.md"), "five")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-06.md"), "six")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-07.md"), "seven")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-08.md"), "eight")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-09.md"), "nine")
	writeFile(t, filepath.Join(root, "episodes", "coder", "2024-01-10.md"), "ten")

	queries := make([]queryRecord, 10)
	for i := range queries {
		queries[i] = queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: []string{"episodes/coder/2024-01-01.md"}}
	}
	resultsDir := t.TempDir()
	if _, err := evaluate(root, queries, stubEmbedder{}, evalOptions{K: 3, Baseline: true, ResultsDir: resultsDir, ProjectSlug: "global", SemanticWeight: 0.5, RecencyWeight: 0.5}); err != nil {
		t.Fatalf("evaluate baseline: %v", err)
	}

	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		t.Fatalf("read results dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one baseline result, got %d", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(resultsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var report evalReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("baseline is not valid JSON: %v", err)
	}
	if report.SchemaVersion != ResultSchemaVersion {
		t.Errorf("baseline schema_version = %d, want result schema %d (independent of the labeled-query schema)", report.SchemaVersion, ResultSchemaVersion)
	}
	if report.ProjectSlug != "global" {
		t.Errorf("baseline project_slug = %q, want global", report.ProjectSlug)
	}
	if report.K != 3 {
		t.Errorf("baseline k = %d, want 3", report.K)
	}
	if report.QueryCount != 10 {
		t.Errorf("baseline query_count = %d, want 10", report.QueryCount)
	}
	// The blend weights must be recorded so the blend metrics are reproducible.
	if !nearly(report.SemanticWeight, 0.5) || !nearly(report.RecencyWeight, 0.5) {
		t.Errorf("baseline weights = %f/%f, want the configured 0.5/0.5", report.SemanticWeight, report.RecencyWeight)
	}
	// Every mode must be present and carry three numeric fields.
	for _, m := range []modeMetrics{report.Modes.Semantic, report.Modes.Recency, report.Modes.Blend} {
		if math.IsNaN(m.Precision) || math.IsNaN(m.Recall) || math.IsNaN(m.MRR) {
			t.Errorf("baseline mode metrics contain NaN: %+v", m)
		}
	}
}

// scoreQuery must surface a scoring error rather than returning zero metrics.
func TestScoreQuery_ErrorPropagation(t *testing.T) {
	searcher := &testSearcher{err: os.ErrPermission}
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "q", Relevant: nil}
	if _, err := scoreQuery(context.Background(), testEmbedder{}, searcher, q, evalPaths, evalOpts()); err == nil {
		t.Fatal("scoreQuery must propagate a search error")
	}
}

// scoreQuery with an empty query is an unscoreable label: it reports an error
// so baseline mode can distinguish it from a genuinely evaluated query.
func TestScoreQuery_EmptyQueryIsUnscoreable(t *testing.T) {
	q := queryRecord{Version: LabeledQuerySchemaVersion, Query: "  ", Relevant: nil}
	if _, err := scoreQuery(context.Background(), testEmbedder{}, &testSearcher{}, q, evalPaths, evalOpts()); err == nil {
		t.Fatal("scoreQuery must report an unscoreable (blank) label as an error")
	}
}

func nearly(a, b float64) bool {
	return math.Abs(a-b) < 0.0000001
}
