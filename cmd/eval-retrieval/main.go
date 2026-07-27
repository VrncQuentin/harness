// Command eval-retrieval computes MRR and Recall@K for the episode retrieval
// system against a labeled query set stored as NDJSON.
//
// Usage:
//
//	eval-retrieval -repo /path/to/project -embedder http://localhost:8082 \
//	               -queries queries.ndjson [-k 5]
//
// Each line of queries.ndjson must be:
//
//	{"query": "...", "relevant": ["episodes/agent/2024-01.md"]}
//
// Outputs per-query MRR and Recall@K, then overall means to stdout.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/embedder"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/memoryops"
)

type queryRecord struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
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
	queriesFile := flag.String("queries", "", "NDJSON file with query+relevant pairs (required)")
	k := flag.Int("k", 5, "Recall@K cutoff (default 5)")
	flag.Parse()

	if *repoPath == "" || *embedderURL == "" || *queriesFile == "" {
		flag.Usage()
		return fmt.Errorf("repo, embedder, and queries are required")
	}

	// The labeled query set is a file the operator names on the command line.
	// It is not inside any harness-managed tree, so there is no root to resolve
	// it through and nothing for a rooted open to contain — see the filesystem
	// access ledger in docs/architecture.md.
	f, err := os.Open(*queriesFile)
	if err != nil {
		return fmt.Errorf("open queries: %w", err)
	}
	defer func() { _ = f.Close() }()

	var queries []queryRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var q queryRecord
		if err := json.Unmarshal(line, &q); err != nil {
			return fmt.Errorf("parse query line: %w", err)
		}
		queries = append(queries, q)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read queries: %w", err)
	}
	if len(queries) == 0 {
		return fmt.Errorf("no queries found in %s", *queriesFile)
	}

	// The repo is pinned once; the index and the episode listing below both go
	// through that handle, exactly as the harness does at runtime.
	mem, err := memory.OpenDirReader(*repoPath)
	if err != nil {
		return fmt.Errorf("open project repo: %w", err)
	}
	defer func() { _ = mem.Close() }()

	episodeIndex, err := memoryops.NewEpisodeIndex(mem, memoryops.EpisodeIndexRootRel)
	if err != nil {
		return fmt.Errorf("open episode index: %w", err)
	}
	defer func() { _ = episodeIndex.Close() }()

	emb := embedder.NewClient(*embedderURL, http.DefaultClient)
	scorer := &memoryops.EpisodeScorer{
		Embedder: emb,
		Config:   config.Defaults().Prompt,
		Index:    episodeIndex,
	}

	paths, err := episodePaths(mem)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no episodes found in %s", *repoPath)
	}

	ctx := context.Background()
	var sumMRR, sumRecall float64
	evaluated := 0
	for i, q := range queries {
		scores, err := scorer.ScoreEpisodes(ctx, q.Query, paths)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query %d: score error: %v\n", i+1, err)
			continue
		}

		// Sort paths by blended score descending.
		ranked := make([]string, len(paths))
		copy(ranked, paths)
		sort.SliceStable(ranked, func(a, b int) bool {
			return scores[ranked[a]].Score > scores[ranked[b]].Score
		})

		relevant := make(map[string]bool, len(q.Relevant))
		for _, r := range q.Relevant {
			relevant[filepath.ToSlash(r)] = true
		}

		// MRR: reciprocal rank of first relevant hit.
		mrr := 0.0
		for rank, p := range ranked {
			if relevant[p] {
				mrr = 1.0 / float64(rank+1)
				break
			}
		}

		// Recall@K: fraction of relevant docs found in top K.
		found := 0
		cutoff := *k
		if cutoff > len(ranked) {
			cutoff = len(ranked)
		}
		for _, p := range ranked[:cutoff] {
			if relevant[p] {
				found++
			}
		}
		recallAtK := 0.0
		if len(q.Relevant) > 0 {
			recallAtK = float64(found) / float64(len(q.Relevant))
		}

		fmt.Printf("query %d: mrr=%.4f recall@%d=%.4f  %q\n", i+1, mrr, *k, recallAtK, q.Query)
		sumMRR += mrr
		sumRecall += recallAtK
		evaluated++
	}

	if evaluated == 0 {
		return fmt.Errorf("no queries could be evaluated")
	}
	n := float64(evaluated)
	fmt.Printf("\nmean MRR=%.4f  mean Recall@%d=%.4f  (n=%d)\n", sumMRR/n, *k, sumRecall/n, evaluated)
	return nil
}

// episodePaths lists every episode markdown file as a repo-relative path,
// enumerated through the pinned repo handle. The scorer keys on repo-relative
// paths, so this never needs an absolute one.
func episodePaths(mem *memory.DirReader) ([]string, error) {
	agents, err := mem.ListDirs("episodes")
	if err != nil {
		return nil, fmt.Errorf("list episode agents: %w", err)
	}
	var paths []string
	for _, agent := range agents {
		matches, err := mem.Glob(path.Join("episodes", agent, "*.md"))
		if err != nil {
			return nil, fmt.Errorf("list episodes for %s: %w", agent, err)
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	return paths, nil
}
