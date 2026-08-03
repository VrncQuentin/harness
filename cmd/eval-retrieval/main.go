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
	"path/filepath"
	"sort"
	"strings"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/embedder"
	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/memoryops"
	"github.com/VrncQuentin/harness/internal/retrieval"
)

type queryRecord struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
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
	queriesFile := flag.String("queries", "", "NDJSON file with query+relevant pairs (required)")
	k := flag.Int("k", 5, "Recall@K cutoff (default 5)")
	flag.Parse()

	if *repoPath == "" || *embedderURL == "" || *queriesFile == "" {
		flag.Usage()
		return fmt.Errorf("repo, embedder, and queries are required")
	}

	queries, err := loadQueries(*queriesFile)
	if err != nil {
		return err
	}
	_, err = evaluate(*repoPath, queries, *k, embedder.NewClient(*embedderURL, http.DefaultClient))
	return err
}

// loadQueries reads the NDJSON query+relevant file.
func loadQueries(path string) ([]queryRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open queries: %w", err)
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
			return nil, fmt.Errorf("parse query line: %w", err)
		}
		queries = append(queries, q)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read queries: %w", err)
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("no queries found in %s", path)
	}
	return queries, nil
}

// evaluate opens the repo through the pinned reader, enumerates episodes
// through it, and scores every query. It is the production scoring path,
// split from run() so tests can drive the same code with a stub embedder:
// reverting enumeration to pathname globs changes what evaluate does. It
// returns the repo-relative paths it scored so a test can assert exactly what
// was enumerated.
func evaluate(repoPath string, queries []queryRecord, k int, emb embedder.Client) ([]string, error) {
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

	scorer := &memoryops.EpisodeScorer{
		Embedder: emb,
		Config:   config.Defaults().Prompt,
		Index:    episodeIndex,
	}

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
	var sumMRR, sumRecall float64
	evaluated := 0
	for i, q := range queries {
		scores, err := scorer.ScoreEpisodes(ctx, retrieval.TraceContext{}, q.Query, paths)
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
		cutoff := k
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

		fmt.Printf("query %d: mrr=%.4f recall@%d=%.4f  %q\n", i+1, mrr, k, recallAtK, q.Query)
		sumMRR += mrr
		sumRecall += recallAtK
		evaluated++
	}

	if evaluated == 0 {
		return nil, fmt.Errorf("no queries could be evaluated")
	}
	n := float64(evaluated)
	fmt.Printf("\nmean MRR=%.4f  mean Recall@%d=%.4f  (n=%d)\n", sumMRR/n, k, sumRecall/n, evaluated)
	return paths, nil
}
