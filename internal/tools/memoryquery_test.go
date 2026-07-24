package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func newMemCallInfo(fn func(context.Context, string, int) ([]MemoryHit, error)) CallInfo {
	return CallInfo{MemoryQuery: fn}
}

func TestMemoryQueryMissingQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	r := tool.Execute(context.Background(), CallInfo{}, map[string]any{})
	if r.Error == "" {
		t.Fatal("expected error for missing query")
	}
}

func TestMemoryQueryEmptyQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	r := tool.Execute(context.Background(), CallInfo{}, map[string]any{"query": "   "})
	if r.Error == "" {
		t.Fatal("expected error for blank query")
	}
}

func TestMemoryQueryNilMemoryQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	r := tool.Execute(context.Background(), CallInfo{}, map[string]any{"query": "hello"})
	if r.Error == "" {
		t.Fatal("expected error when MemoryQuery is nil")
	}
	if !strings.Contains(r.Error, "retrieval unavailable") {
		t.Errorf("unexpected error message: %s", r.Error)
	}
}

func TestMemoryQueryNoHits(t *testing.T) {
	tool := &memoryQueryTool{}
	ci := newMemCallInfo(func(_ context.Context, _ string, _ int) ([]MemoryHit, error) {
		return nil, nil
	})
	r := tool.Execute(context.Background(), ci, map[string]any{"query": "no match"})
	if r.Error != "" {
		t.Fatalf("unexpected error: %s", r.Error)
	}
	if !strings.Contains(r.Content, "No relevant") {
		t.Errorf("unexpected content: %q", r.Content)
	}
}

func TestMemoryQueryFormatsHits(t *testing.T) {
	tool := &memoryQueryTool{}
	hits := []MemoryHit{
		{Path: "episodes/agent/2025-01.md", Score: 0.9, Excerpt: "First episode content"},
		{Path: "episodes/agent/2025-02.md", Score: 0.7, Excerpt: "Second episode content"},
	}
	ci := newMemCallInfo(func(_ context.Context, _ string, _ int) ([]MemoryHit, error) {
		return hits, nil
	})
	r := tool.Execute(context.Background(), ci, map[string]any{"query": "agent episode"})
	if r.Error != "" {
		t.Fatalf("unexpected error: %s", r.Error)
	}
	if !strings.Contains(r.Content, "## Hit 1") {
		t.Error("missing Hit 1 header")
	}
	if !strings.Contains(r.Content, "## Hit 2") {
		t.Error("missing Hit 2 header")
	}
	if !strings.Contains(r.Content, "episodes/agent/2025-01.md") {
		t.Error("missing episode path in output")
	}
	if r.Origin != OriginExtraction {
		t.Errorf("origin: want OriginExtraction, got %v", r.Origin)
	}
}

func TestMemoryQueryKDefault(t *testing.T) {
	tool := &memoryQueryTool{}
	var receivedK int
	ci := newMemCallInfo(func(_ context.Context, _ string, k int) ([]MemoryHit, error) {
		receivedK = k
		return nil, nil
	})
	tool.Execute(context.Background(), ci, map[string]any{"query": "x"})
	if receivedK != memoryQueryDefaultK {
		t.Errorf("default K: want %d, got %d", memoryQueryDefaultK, receivedK)
	}
}

func TestMemoryQueryKCapped(t *testing.T) {
	tool := &memoryQueryTool{}
	var receivedK int
	ci := newMemCallInfo(func(_ context.Context, _ string, k int) ([]MemoryHit, error) {
		receivedK = k
		return nil, nil
	})
	tool.Execute(context.Background(), ci, map[string]any{"query": "x", "k": float64(999)})
	if receivedK != memoryQueryMaxK {
		t.Errorf("capped K: want %d, got %d", memoryQueryMaxK, receivedK)
	}
}

func TestMemoryQueryErrorPropagation(t *testing.T) {
	tool := &memoryQueryTool{}
	ci := newMemCallInfo(func(_ context.Context, _ string, _ int) ([]MemoryHit, error) {
		return nil, errors.New("embedder offline")
	})
	r := tool.Execute(context.Background(), ci, map[string]any{"query": "x"})
	if !strings.Contains(r.Error, "embedder offline") {
		t.Errorf("error not propagated: %s", r.Error)
	}
}

func TestMemoryQueryToolID(t *testing.T) {
	tool := &memoryQueryTool{}
	if tool.ID() != "memory_query" {
		t.Errorf("ID: want memory_query, got %s", tool.ID())
	}
}

func TestMemoryQuerySchema(t *testing.T) {
	tool := &memoryQueryTool{}
	schema := tool.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["query"]; !ok {
		t.Error("schema missing query property")
	}
	req, ok := schema["required"].([]string)
	if !ok || len(req) == 0 || req[0] != "query" {
		t.Error("schema: query must be required")
	}
}
