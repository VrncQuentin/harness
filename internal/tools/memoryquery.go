package tools

import (
	"context"
	"fmt"
	"strings"
)

const (
	memoryQueryDefaultK = 5
	memoryQueryMaxK     = 20
	memoryQueryExcerpt  = 300
)

// memoryQueryTool performs blended semantic+recency retrieval over the active
// project's episode store. Tier-1: read-only, no approval gate.
type memoryQueryTool struct{}

var _ Tool = (*memoryQueryTool)(nil)

func (t *memoryQueryTool) ID() string { return "memory_query" }

func (t *memoryQueryTool) Description() string {
	return "Retrieves the most relevant past episodes from the active project's memory " +
		"using blended semantic and recency scoring. Returns up to k scored hits with excerpts. " +
		"Requires the embedder to be running."
}

func (t *memoryQueryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Text to search for in past episodes",
			},
			"k": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum results to return (1–%d, default %d)", memoryQueryMaxK, memoryQueryDefaultK),
			},
		},
		"required": []string{"query"},
	}
}

func (t *memoryQueryTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return Result{Error: "memory_query: query is required"}
	}
	query = strings.TrimSpace(query)

	k := memoryQueryDefaultK
	if kv, ok := args["k"].(float64); ok && kv > 0 {
		k = int(kv)
	}
	if k < 1 {
		k = 1
	}
	if k > memoryQueryMaxK {
		k = memoryQueryMaxK
	}

	if c.MemoryQuery == nil {
		return Result{Error: "memory_query: retrieval unavailable (embedder may not be running)"}
	}

	hits, err := c.MemoryQuery(ctx, query, k)
	if err != nil {
		return Result{Error: "memory_query: " + err.Error()}
	}
	if len(hits) == 0 {
		return Result{Content: "No relevant episodes found.", Origin: OriginExtraction}
	}

	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "## Hit %d — %s (score %.4f)\n\n%s\n\n", i+1, h.Path, h.Score, h.Excerpt)
	}
	return Result{Content: strings.TrimSpace(sb.String()), Origin: OriginExtraction}
}
