package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMemoryQueryTool_EmptyQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{"query": "  "})
	if res.Error == "" {
		t.Fatal("expected error for blank query")
	}
}

func TestMemoryQueryTool_MissingQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	res := tool.Execute(context.Background(), CallInfo{}, map[string]any{})
	if res.Error == "" {
		t.Fatal("expected error for missing query key")
	}
}

func TestMemoryQueryTool_NilMemoryQuery(t *testing.T) {
	tool := &memoryQueryTool{}
	res := tool.Execute(context.Background(), CallInfo{MemoryQuery: nil}, map[string]any{"query": "something"})
	if res.Error == "" {
		t.Fatal("expected error when MemoryQuery is nil")
	}
	if !strings.Contains(res.Error, "unavailable") {
		t.Errorf("error %q should mention 'unavailable'", res.Error)
	}
}

func TestMemoryQueryTool_NoHits(t *testing.T) {
	tool := &memoryQueryTool{}
	fn := func(_ context.Context, _ string, _ int) ([]MemoryHit, error) { return nil, nil }
	res := tool.Execute(context.Background(), CallInfo{MemoryQuery: fn}, map[string]any{"query": "topic"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Content != "No episodes found." {
		t.Errorf("got %q, want 'No episodes found.'", res.Content)
	}
}

func TestMemoryQueryTool_FormatsHits(t *testing.T) {
	tool := &memoryQueryTool{}
	fn := func(_ context.Context, _ string, _ int) ([]MemoryHit, error) {
		return []MemoryHit{
			{Path: "episodes/agent/2024-01.md", Score: 0.9, Content: "first episode"},
			{Path: "episodes/agent/2024-02.md", Score: 0.7, Content: "second episode"},
		}, nil
	}
	res := tool.Execute(context.Background(), CallInfo{MemoryQuery: fn}, map[string]any{"query": "topic"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	for _, want := range []string{"[1]", "[2]", "first episode", "second episode", "0.9000", "0.7000"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("output missing %q:\n%s", want, res.Content)
		}
	}
}

func TestMemoryQueryTool_KDefault(t *testing.T) {
	tool := &memoryQueryTool{}
	var gotK int
	fn := func(_ context.Context, _ string, k int) ([]MemoryHit, error) {
		gotK = k
		return nil, nil
	}
	tool.Execute(context.Background(), CallInfo{MemoryQuery: fn}, map[string]any{"query": "q"})
	if gotK != 5 {
		t.Errorf("default k = %d, want 5", gotK)
	}
}

func TestMemoryQueryTool_KCappedAt20(t *testing.T) {
	tool := &memoryQueryTool{}
	var gotK int
	fn := func(_ context.Context, _ string, k int) ([]MemoryHit, error) {
		gotK = k
		return nil, nil
	}
	tool.Execute(context.Background(), CallInfo{MemoryQuery: fn}, map[string]any{"query": "q", "k": float64(99)})
	if gotK != 20 {
		t.Errorf("k = %d, want 20", gotK)
	}
}

func TestMemoryQueryTool_PropagatesError(t *testing.T) {
	tool := &memoryQueryTool{}
	fn := func(_ context.Context, _ string, _ int) ([]MemoryHit, error) {
		return nil, errors.New("index corrupt")
	}
	res := tool.Execute(context.Background(), CallInfo{MemoryQuery: fn}, map[string]any{"query": "q"})
	if !strings.Contains(res.Error, "index corrupt") {
		t.Errorf("error %q should contain 'index corrupt'", res.Error)
	}
}

func TestMemoryQueryTool_IDAndSchema(t *testing.T) {
	tool := &memoryQueryTool{}
	if tool.ID() != "memory_query" {
		t.Errorf("ID() = %q, want 'memory_query'", tool.ID())
	}
	schema := tool.Schema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema missing 'properties'")
	}
	if _, ok := props["query"]; !ok {
		t.Error("schema properties missing 'query'")
	}
	if _, ok := props["k"]; !ok {
		t.Error("schema properties missing 'k'")
	}
	req, ok := schema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Errorf("schema required = %v, want [query]", schema["required"])
	}
}
