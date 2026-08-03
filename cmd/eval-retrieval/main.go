// Command eval-retrieval measures episode retrieval quality against a labeled
// query set stored as NDJSON.
//
// Usage:
//
//	eval-retrieval -repo /path/to/project -embedder http://localhost:8082 \
//	               -queries queries.ndjson [-k 3] [-baseline]
//
// Each line of queries.ndjson must be a versioned labeled-query row:
//
//	{"version":1,"query":"...","relevant":["episodes/agent/2024-01.md"]}
//
// version is the labeled-query schema version, independent of the operational
// trace schema (internal/retrieval). A row whose version the evaluator does
// not recognize is rejected rather than guessed at.
//
// For every query the harness replays the query against the selected project
// repo and reports Precision@K and Recall@K for three signals independently:
// semantic-only (weight 1,0), recency-only (weight 0,1), and the configured
// blend (the semantic/recency weights passed on the command line). MRR is
// reported as an additional diagnostic, not a substitute.
//
// Baseline mode (-baseline) requires at least ten valid rows, evaluates all of
// them, and writes a machine-readable result document to
// ~/.harness/eval/retrieval/results/<project-slug>-<timestamp>.json.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/embedder"
	"github.com/VrncQuentin/harness/internal/home"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/memoryops"
	"github.com/VrncQuentin/harness/internal/retrieval"
	"github.com/VrncQuentin/harness/internal/rootfs"
)

// LabeledQuerySchemaVersion is the schema version of the labeled-query file.
// It is deliberately independent of retrieval.TraceSchemaVersion: the two
// artifacts version separately, and the evaluator rejects a row whose version
// it does not recognize.
const LabeledQuerySchemaVersion = 1

// queryRecord is one versioned labeled query.
type queryRecord struct {
	Version  int      `json:"version"`
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

// modeKind names one evaluated retrieval signal.
type modeKind string

const (
	modeSemantic modeKind = "semantic"
	modeRecency  modeKind = "recency"
	modeBlend    modeKind = "blend"
)

// modeMetrics holds the aggregate metrics for one mode across all queries.
type modeMetrics struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	MRR       float64 `json:"mrr"`
}

// modesReport is the machine-readable result document for one evaluation run.
type modesReport struct {
	Semantic modeMetrics `json:"semantic"`
	Recency  modeMetrics `json:"recency"`
	Blend    modeMetrics `json:"blend"`
}

// evalReport is the stable machine-readable baseline artifact written in
// baseline mode.
type evalReport struct {
	SchemaVersion int         `json:"schema_version"`
	ProjectSlug   string      `json:"project_slug"`
	GeneratedAt   time.Time   `json:"generated_at"`
	K             int         `json:"k"`
	QueryCount    int         `json:"query_count"`
	Modes         modesReport `json:"modes"`
}

// evalOptions carries the tunable evaluation parameters. The configured blend
// is the semantic/recency weight pair the production harness would use.
type evalOptions struct {
	K              int
	ProjectSlug    string
	SemanticWeight float64
	RecencyWeight  float64
	Baseline       bool
	ResultsDir     string
}

// episodePaths returns the repo-relative forward-slash paths of every episode
// file under the pinned repo reader. The walk resolves each component through
// the anchored handle — the historical episodes/<agent>/<file>.md shape, no
// pathname is ever re-resolved outside the pinned tree, and a link leaving the
// tree fails the walk closed.
func episodePaths(repo *memory.DirReader) ([]string, error) {
	entries, err := repo.Walk("episodes")
	if err != nil {
		return nil, fmt.Errorf("walk episodes: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Dir || !strings.HasSuffix(e.Path, ".md") {
			continue
		}
		// Preserve the historical depth-two shape episodes/<agent>/<file>.md.
		if len(strings.Split(e.Path, "/")) != 3 {
			continue
		}
		paths = append(paths, e.Path)
	}
	sort.Strings(paths)
	return paths, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "eval-retrieval:", err)
		os.Exit(1)
	}
}

func run() error {
	repoPath := flag.String("repo", "", "Path to project memory repo (required)")
	embedderURL := flag.String("embedder", "", "Embedder HTTP base URL, e.g. http://localhost:8082 (required)")
	queriesFile := flag.String("queries", "", "NDJSON file with versioned query+relevant rows (required)")
	k := flag.Int("k", 3, "Precision@K / Recall@K cutoff (default 3)")
	projectSlug := flag.String("project", "global", "Project slug, used in the baseline result filename")
	semanticWeight := flag.Float64("semantic-weight", config.Defaults().Prompt.SemanticWeight, "Configured semantic weight for the blend mode")
	recencyWeight := flag.Float64("recency-weight", config.Defaults().Prompt.RecencyWeight, "Configured recency weight for the blend mode")
	baseline := flag.Bool("baseline", false, "Require at least ten valid rows and write a machine-readable result")
	resultsDir := flag.String("results-dir", "", "Baseline results directory (default ~/.harness/eval/retrieval/results)")
	flag.Parse()

	if *repoPath == "" || *embedderURL == "" || *queriesFile == "" {
		flag.Usage()
		return fmt.Errorf("repo, embedder, and queries are required")
	}

	queries, err := loadQueries(*queriesFile)
	if err != nil {
		return err
	}

	dir := *resultsDir
	if dir == "" {
		hh, herr := home.Default()
		if herr != nil {
			return fmt.Errorf("resolve harness home: %w", herr)
		}
		dir = filepath.Join(hh, "eval", "retrieval", "results")
	}

	opts := evalOptions{
		K:              *k,
		ProjectSlug:    *projectSlug,
		SemanticWeight: *semanticWeight,
		RecencyWeight:  *recencyWeight,
		Baseline:       *baseline,
		ResultsDir:     dir,
	}
	_, err = evaluate(*repoPath, queries, embedder.NewClient(*embedderURL, http.DefaultClient), opts)
	return err
}

// loadQueries reads the versioned NDJSON query+relevant file. Every row must
// carry LabeledQuerySchemaVersion; a row with an unknown or missing version is
// rejected with the offending row number and version, never guessed at.
func loadQueries(path string) ([]queryRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open queries: %w", err)
	}
	defer func() { _ = f.Close() }()

	var queries []queryRecord
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var q queryRecord
		if err := json.Unmarshal(line, &q); err != nil {
			return nil, fmt.Errorf("row %d: parse query line: %w", lineNo, err)
		}
		if q.Version != LabeledQuerySchemaVersion {
			return nil, fmt.Errorf("row %d: unsupported labeled-query schema version %d (supported: %d)", lineNo, q.Version, LabeledQuerySchemaVersion)
		}
		queries = append(queries, q)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read queries: %w", err)
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("no valid queries found in %s", path)
	}
	return queries, nil
}

// evaluate opens the repo through the pinned reader, enumerates episodes
// through it, and scores every query under all three signals. It is the
// production scoring path, split from run() so tests can drive the same code
// with a stub embedder: reverting enumeration to pathname globs changes what
// evaluate does. It returns the repo-relative paths it scored so a test can
// assert exactly what was enumerated.
func evaluate(repoPath string, queries []queryRecord, emb embedder.Client, opts evalOptions) ([]string, error) {
	if opts.Baseline && len(queries) < 10 {
		return nil, fmt.Errorf("baseline requires at least 10 valid queries, got %d", len(queries))
	}
	if opts.K <= 0 {
		return nil, fmt.Errorf("k must be positive, got %d", opts.K)
	}

	repoDirReader, err := memory.NewDirReader(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}
	defer func() { _ = repoDirReader.Close() }()
	indexDir := memoryops.EpisodeIndexDir(repoPath)
	if err := repoDirReader.MkdirAll("index/_episodes"); err != nil {
		return nil, fmt.Errorf("mkdir index: %w", err)
	}
	indexAnchor, err := repoDirReader.SubAnchor("index/_episodes")
	if err != nil {
		return nil, fmt.Errorf("index anchor: %w", err)
	}
	episodeIndex, err := memoryops.NewEpisodeIndex(indexAnchor, indexDir, repoDirReader.Identity())
	if err != nil {
		_ = indexAnchor.Close()
		return nil, fmt.Errorf("open episode index: %w", err)
	}
	defer func() { _ = episodeIndex.Close() }()

	// Enumerate episode files through the pinned repo reader rather than by
	// pathname. The walk resolves every component against the anchored handle,
	// refuses links that leave the tree, and returns stable repo-relative
	// forward-slash paths — the same shape the scorer and the index expect.
	paths, err := episodePaths(repoDirReader)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no episodes found in %s", repoPath)
	}

	ctx := context.Background()
	var sum [3]modeMetrics
	evaluated := 0
	for i, q := range queries {
		metrics, err := scoreQuery(ctx, emb, episodeIndex, q, paths, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query %d: score error: %v\n", i+1, err)
			continue
		}
		fmt.Printf("query %d: %q\n", i+1, q.Query)
		fmt.Printf("  semantic p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, metrics.Semantic.Precision, opts.K, metrics.Semantic.Recall, metrics.Semantic.MRR)
		fmt.Printf("  recency  p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, metrics.Recency.Precision, opts.K, metrics.Recency.Recall, metrics.Recency.MRR)
		fmt.Printf("  blend    p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, metrics.Blend.Precision, opts.K, metrics.Blend.Recall, metrics.Blend.MRR)
		for mi, m := range [3]modeMetrics{metrics.Semantic, metrics.Recency, metrics.Blend} {
			sum[mi].Precision += m.Precision
			sum[mi].Recall += m.Recall
			sum[mi].MRR += m.MRR
		}
		evaluated++
	}

	if evaluated == 0 {
		return nil, fmt.Errorf("no queries could be evaluated")
	}

	n := float64(evaluated)
	report := evalReport{
		SchemaVersion: LabeledQuerySchemaVersion,
		ProjectSlug:   opts.ProjectSlug,
		GeneratedAt:   time.Now().UTC(),
		K:             opts.K,
		QueryCount:    evaluated,
	}
	report.Modes.Semantic = avgMode(sum[0], n)
	report.Modes.Recency = avgMode(sum[1], n)
	report.Modes.Blend = avgMode(sum[2], n)

	fmt.Printf("\nmean (n=%d)\n", evaluated)
	fmt.Printf("  semantic p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, report.Modes.Semantic.Precision, opts.K, report.Modes.Semantic.Recall, report.Modes.Semantic.MRR)
	fmt.Printf("  recency  p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, report.Modes.Recency.Precision, opts.K, report.Modes.Recency.Recall, report.Modes.Recency.MRR)
	fmt.Printf("  blend    p@%d=%.4f r@%d=%.4f mrr=%.4f\n", opts.K, report.Modes.Blend.Precision, opts.K, report.Modes.Blend.Recall, report.Modes.Blend.MRR)

	if opts.Baseline {
		if err := writeBaseline(report, opts.ResultsDir); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// scoreQuery scores one query under the three signals and computes per-mode
// Precision@K, Recall@K, and MRR. Semantic scores come from the searcher (the
// episode index in production, a stub in tests); recency is always derived from
// the oldest-first episode order, so recency-only remains evaluable even when
// the index returns no semantic hits.
func scoreQuery(ctx context.Context, emb retrieval.EpisodeEmbedder, searcher retrieval.EpisodeSearcher, q queryRecord, paths []string, opts evalOptions) (modesReport, error) {
	if strings.TrimSpace(q.Query) == "" {
		return modesReport{}, nil
	}
	vecs, err := emb.Embed(ctx, []string{q.Query})
	if err != nil {
		return modesReport{}, err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return modesReport{}, nil
	}
	results, err := searcher.Search(vecs[0], len(paths)*2)
	if err != nil {
		return modesReport{}, err
	}

	oldestFirst := append([]string(nil), paths...)
	sort.Strings(oldestFirst)
	semanticScores := retrieval.BestSemanticScores(results)

	relevant := make(map[string]bool, len(q.Relevant))
	for _, r := range q.Relevant {
		relevant[filepath.ToSlash(r)] = true
	}

	weights := []struct {
		mode modeKind
		sw   float64
		rw   float64
	}{
		{modeSemantic, 1, 0},
		{modeRecency, 0, 1},
		{modeBlend, opts.SemanticWeight, opts.RecencyWeight},
	}
	var report modesReport
	for _, w := range weights {
		scored := retrieval.BlendEpisodeScores(oldestFirst, semanticScores, w.sw, w.rw)
		ranked := rankByScore(paths, scored)
		m := metricsFor(ranked, relevant, opts.K)
		switch w.mode {
		case modeSemantic:
			report.Semantic = m
		case modeRecency:
			report.Recency = m
		case modeBlend:
			report.Blend = m
		}
	}
	return report, nil
}

// rankByScore orders paths by blended score descending, stable so equal scores
// keep the original (oldest-first) episode order.
func rankByScore(paths []string, scores map[string]float64) []string {
	ranked := make([]string, len(paths))
	copy(ranked, paths)
	sort.SliceStable(ranked, func(a, b int) bool {
		return scores[ranked[a]] > scores[ranked[b]]
	})
	return ranked
}

// metricsFor computes Precision@K, Recall@K, and MRR for one ranked list.
// Precision@K is |relevant ∩ top-K| / min(K, |top-K|) so a corpus smaller than
// K is not penalized for results it cannot produce. Recall@K is
// |relevant ∩ top-K| / |relevant|, defined as 0 when there are no relevant
// episodes. MRR is the reciprocal rank of the first relevant hit, 0 when none.
func metricsFor(ranked []string, relevant map[string]bool, k int) modeMetrics {
	var m modeMetrics
	if k > len(ranked) {
		k = len(ranked)
	}
	found := 0
	firstRelevant := 0
	for i, p := range ranked[:k] {
		if relevant[p] {
			found++
			if firstRelevant == 0 {
				firstRelevant = i + 1
			}
		}
	}
	m.Precision = float64(found) / float64(k)
	if len(relevant) > 0 {
		m.Recall = float64(found) / float64(len(relevant))
	}
	if firstRelevant > 0 {
		m.MRR = 1.0 / float64(firstRelevant)
	}
	return m
}

func avgMode(m modeMetrics, n float64) modeMetrics {
	return modeMetrics{
		Precision: m.Precision / n,
		Recall:    m.Recall / n,
		MRR:       m.MRR / n,
	}
}

// writeBaseline writes the machine-readable result document under the results
// directory using a pinned rooted handle: <slug>-<timestamp>.json. A failed
// write is surfaced, not silently dropped.
func writeBaseline(report evalReport, dir string) error {
	root, err := rootfs.OpenOrCreate(dir, 0o755)
	if err != nil {
		return fmt.Errorf("open baseline results dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	name := fmt.Sprintf("%s-%s.json", report.ProjectSlug, report.GeneratedAt.UTC().Format("2006-01-02T15-04-05Z"))
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode baseline: %w", err)
	}
	b = append(b, '\n')
	if err := root.WriteStreamAtomic(name, bytes.NewReader(b), 0o644); err != nil {
		return fmt.Errorf("write baseline %s: %w", name, err)
	}
	slog.Info("baseline written", "path", filepath.Join(dir, name))
	return nil
}
