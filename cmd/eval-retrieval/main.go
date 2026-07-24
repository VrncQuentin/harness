// eval-retrieval runs a labeled query set against the retrieval stack and
// prints MRR (Mean Reciprocal Rank) and Recall@K for each query and overall.
//
// Usage:
//
//	eval-retrieval -repo /path/to/project -embedder http://localhost:8082 \
//	               -queries queries.ndjson [-k 5]
//
// queries.ndjson format (one JSON object per line):
//
//	{"query": "...", "relevant": ["episodes/agent/2024-01.md"]}
//
// The embedder sidecar must be running at the given URL. The episode index
// must already exist under <repo>/index/_episodes/.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vrnc/harness/internal/embedder"
	"github.com/vrnc/harness/internal/memoryops"
	"github.com/vrnc/harness/internal/retrieval"
)

type labeledQuery struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

type queryResult struct {
	Query     string
	Relevant  []string
	Ranked    []string // episode paths, highest score first
	RR        float64  // reciprocal rank (0 if no relevant found in top-K)
	RecallAtK float64
}

func main() {
	repo := flag.String("repo", "", "path to the project memory repo (required)")
	embedURL := flag.String("embedder", "http://localhost:8082", "embedder sidecar base URL")
	queriesFile := flag.String("queries", "", "NDJSON file of labeled queries (required)")
	k := flag.Int("k", 5, "top-K cutoff for Recall@K and MRR")
	flag.Parse()

	if *repo == "" || *queriesFile == "" {
		flag.Usage()
		os.Exit(1)
	}

	queries, err := loadQueries(*queriesFile)
	if err != nil {
		log.Fatalf("load queries: %v", err)
	}
	if len(queries) == 0 {
		log.Fatal("no queries loaded")
	}

	embedClient := embedder.NewClient(*embedURL, nil)
	indexDir := memoryops.EpisodeIndexDir(*repo)
	episodeIndex, err := memoryops.NewEpisodeIndex(indexDir)
	if err != nil {
		log.Fatalf("open episode index: %v", err)
	}

	episodePaths, err := globEpisodes(*repo)
	if err != nil {
		log.Fatalf("glob episodes: %v", err)
	}
	if len(episodePaths) == 0 {
		log.Fatalf("no episodes found under %s", *repo)
	}

	ctx := context.Background()
	results := make([]queryResult, 0, len(queries))

	for _, q := range queries {
		scores, ok, err := retrieval.ScoreEpisodePaths(ctx, embedClient, episodeIndex.Current(), q.Query, episodePaths, 0.7, 0.3, nil)
		if err != nil {
			log.Printf("score %q: %v (skipped)", q.Query, err)
			continue
		}
		var ranked []string
		if ok {
			ranked = sortedByScore(scores)
		}

		rr, recall := evalMetrics(ranked, q.Relevant, *k)
		results = append(results, queryResult{
			Query:     q.Query,
			Relevant:  q.Relevant,
			Ranked:    ranked,
			RR:        rr,
			RecallAtK: recall,
		})
	}

	printReport(results, *k)
}

func loadQueries(path string) ([]labeledQuery, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []labeledQuery
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var q labeledQuery
		if err := json.Unmarshal([]byte(line), &q); err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		if q.Query != "" {
			out = append(out, q)
		}
	}
	return out, sc.Err()
}

func globEpisodes(repoRoot string) ([]string, error) {
	pattern := filepath.Join(repoRoot, "episodes", "*", "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	// Normalise to forward slashes for consistent comparison with query labels.
	for i, m := range matches {
		matches[i] = strings.ReplaceAll(m, "\\", "/")
	}
	return matches, nil
}

func sortedByScore(scores map[string]float64) []string {
	type kv struct {
		path  string
		score float64
	}
	pairs := make([]kv, 0, len(scores))
	for p, s := range scores {
		pairs = append(pairs, kv{path: p, score: s})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].score != pairs[j].score {
			return pairs[i].score > pairs[j].score
		}
		return pairs[i].path < pairs[j].path
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.path
	}
	return out
}

// evalMetrics returns the reciprocal rank and Recall@K for one query.
// ranked is the full scored list; relevant is the gold set; k is the cutoff.
func evalMetrics(ranked, relevant []string, k int) (rr, recall float64) {
	rel := make(map[string]bool, len(relevant))
	for _, r := range relevant {
		rel[normalise(r)] = true
	}

	topK := ranked
	if len(topK) > k {
		topK = topK[:k]
	}

	var found int
	for i, p := range topK {
		if rel[normalise(p)] {
			found++
			if rr == 0 {
				rr = 1.0 / float64(i+1)
			}
		}
	}
	if len(relevant) > 0 {
		recall = float64(found) / float64(len(relevant))
	}
	return rr, recall
}

func normalise(p string) string {
	return strings.TrimSuffix(strings.ReplaceAll(p, "\\", "/"), ".md")
}

func printReport(results []queryResult, k int) {
	var sumMRR, sumRecall float64
	fmt.Printf("%-60s  %6s  %9s\n", "Query", "RR", fmt.Sprintf("Recall@%d", k))
	fmt.Println(strings.Repeat("-", 80))
	for _, r := range results {
		q := r.Query
		if len(q) > 57 {
			q = q[:57] + "..."
		}
		fmt.Printf("%-60s  %6.4f  %9.4f\n", q, r.RR, r.RecallAtK)
		sumMRR += r.RR
		sumRecall += r.RecallAtK
	}
	n := float64(len(results))
	if n == 0 {
		fmt.Println("no results")
		return
	}
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("%-60s  %6.4f  %9.4f\n", fmt.Sprintf("MEAN (n=%d)", len(results)), sumMRR/n, sumRecall/n)
}
