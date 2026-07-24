package tools

import (
	"context"
	"fmt"
	"strings"
)

// memoryQueryTool implements the memory_query tool: ranked episode retrieval.
// It emits a D3 trace row per candidate via the runtime-injected MemoryQueryFn,
// which calls ScoreEpisodePaths under the hood.
type memoryQueryTool struct{}

var _ Tool = (*memoryQueryTool)(nil)

func (t *memoryQueryTool) ID() string { return "memory_query" }

func (t *memoryQueryTool) Description() string {
	return "Retrieve the most relevant past episodes from the active project memory using " +
		"blended semantic + recency scoring. Returns episode paths, scores, and content " +
		"for the top-k candidates. Emits a D3 trace row per candidate for retrieval evaluation."
}

func (t *memoryQueryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query used to rank episodes by semantic relevance",
			},
			"k": map[string]any{
				"type":        "integer",
				"description": "Maximum number of episodes to return (default 5, max 20)",
				"minimum":     1,
				"maximum":     20,
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
	k := 5
	if kRaw, ok := args["k"].(float64); ok && kRaw > 0 {
		k = int(kRaw)
		if k > 20 {
			k = 20
		}
	}

	if c.MemoryQuery == nil {
		return Result{Error: "memory_query: retrieval unavailable (assembler not wired)"}
	}
	hits, err := c.MemoryQuery(ctx, query, k)
	if err != nil {
		return Result{Error: fmt.Sprintf("memory_query: %v", err)}
	}
	if len(hits) == 0 {
		return Result{Content: "No episodes found."}
	}

	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "## [%d] %s  (score: %.4f)\n\n%s\n\n", i+1, h.Path, h.Score, h.Content)
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}
}
