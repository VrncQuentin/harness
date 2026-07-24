package retrieval

import (
	"crypto/sha256"
	"fmt"
)

// RetrievalTrace is one row per candidate per ScoreEpisodePaths call.
// query_id is a hash of the query text so no raw query strings or PII land
// in trace rows. The row schema is stable: new signals add fields, existing
// fields stay put.
type RetrievalTrace struct {
	QueryID   string  // SHA-256[:8] of the query text
	Candidate string  // episode path
	Semantic  float64 // raw semantic similarity score (0 if embedder miss)
	Recency   float64 // raw exp_decay value
	SWeight   float64 // semantic_weight at call time
	RWeight   float64 // recency_weight at call time
	Score     float64 // final blended score
	Returned  bool    // whether this candidate was in the top-K result
}

// TraceSink receives per-candidate trace rows from ScoreEpisodePaths.
// Implementations must be safe for concurrent use.
type TraceSink interface {
	Emit(RetrievalTrace)
}

// NopTraceSink is a no-op sink used when tracing is not configured.
type NopTraceSink struct{}

func (NopTraceSink) Emit(RetrievalTrace) {}

// QueryID returns a short deterministic identifier for a query string.
// The raw text is not stored — only this hash appears in trace rows.
func QueryID(query string) string {
	h := sha256.Sum256([]byte(query))
	return fmt.Sprintf("%x", h[:8])
}
