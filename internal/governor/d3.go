package governor

// LabeledQuery is one entry in the D3 retrieval-quality labeled set.
// Files live under ~/.harness/eval/retrieval/ as JSON.
// The eval binary (cmd/eval-retrieval, M10.3) reads these files and
// computes precision@k / recall@k against retrieval traces.
//
// Labels accumulate from real work: when a memory_query call returns
// a correct result the operator confirms, that (query, episode_paths)
// pair becomes a labeled row. The full contract — trace storage format,
// query_id hashing, eval binary — is specified in memory_roadmap.md MR0.
type LabeledQuery struct {
	// Query is the raw query string.
	Query string `json:"query"`
	// ExpectedEpisodePaths lists the episode paths that should be returned
	// for this query, in any order.
	ExpectedEpisodePaths []string `json:"expected_episode_paths"`
}
