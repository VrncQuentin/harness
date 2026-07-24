package governor

import (
	"context"
	"strings"
	"testing"

	"github.com/vrnc/harness/internal/parser"
	"github.com/vrnc/harness/internal/tools"
)

func mustRegistry(t *testing.T) *parser.Registry {
	t.Helper()
	r, err := parser.NewRegistry(parser.NewGoFrontEnd())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// --- B1 tests ---

func TestB1_NonReadTool(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "file_list", nil, tools.Result{Content: "hello"}, "foo bar")
	if res.Content != "hello" {
		t.Fatalf("B1 should not fire for non-read tools, got %q", res.Content)
	}
}

func TestB1_UnsupportedExtension(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.py"},
		tools.Result{Content: "def foo(): pass"},
		"foo")
	if res.Content != "def foo(): pass" {
		t.Fatalf("B1 should not fire for unsupported extension, got %q", res.Content)
	}
}

func TestB1_LocatorRead_Skipped(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.go", "locator": "main.go:1-5"},
		tools.Result{Content: "package main"},
		"main")
	if res.Content != "package main" {
		t.Fatalf("B1 should not fire for locator reads, got %q", res.Content)
	}
}

func TestB1_RangeRead_Skipped(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.go", "start_line": float64(1)},
		tools.Result{Content: "package main"},
		"main")
	if res.Content != "package main" {
		t.Fatalf("B1 should not fire for range reads, got %q", res.Content)
	}
}

const goSrc = `package main

func RelevantFunc(x int) int {
	return x + 1
}

func IrrelevantFunc(y string) string {
	return y + "!"
}
`

func TestB1_SkeletonizesIrrelevantBodies(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.go"},
		tools.Result{Content: goSrc},
		"relevant")
	// Relevant function body should be present.
	if !strings.Contains(res.Content, "return x + 1") {
		t.Fatalf("relevant body missing: %q", res.Content)
	}
	// Irrelevant function body should be skeletonized.
	if strings.Contains(res.Content, `return y + "!"`) {
		t.Fatalf("irrelevant body should be skeletonized: %q", res.Content)
	}
	// Stub marker should appear.
	if !strings.Contains(res.Content, "skeletonized") {
		t.Fatalf("expected skeletonization stub, got %q", res.Content)
	}
}

func TestB1_AllRelevant_Unchanged(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.go"},
		tools.Result{Content: goSrc},
		"relevant irrelevant")
	// Both bodies match query tokens, so no change.
	if res.Content != goSrc {
		t.Fatalf("all-relevant: expected unchanged content, got %q", res.Content)
	}
}

func TestB1_NoQuery_Unchanged(t *testing.T) {
	g := New(mustRegistry(t), t.TempDir())
	res := g.Apply(context.Background(), "read",
		map[string]any{"path": "main.go"},
		tools.Result{Content: goSrc},
		"")
	if res.Content != goSrc {
		t.Fatalf("no query: expected unchanged content, got %q", res.Content)
	}
}

// --- B3 tests ---

func TestB3_ShortError_Unchanged(t *testing.T) {
	g := New(nil, t.TempDir())
	res := g.Apply(context.Background(), "exec", nil,
		tools.Result{Error: "short error"},
		"")
	if res.Error != "short error" {
		t.Fatalf("short error should not be spilled, got %q", res.Error)
	}
}

func TestB3_LargeError_Spilled(t *testing.T) {
	g := New(nil, t.TempDir())
	bigErr := strings.Repeat("x", b3Threshold+1)
	res := g.Apply(context.Background(), "exec", nil,
		tools.Result{Error: bigErr},
		"")
	if !strings.Contains(res.Error, "toolout:") {
		t.Fatalf("large error should produce toolout handle, got %q", res.Error)
	}
	if len(res.Error) >= b3Threshold {
		t.Fatalf("B3 should have truncated the error, len=%d", len(res.Error))
	}
}

func TestB3_NoCacheDir_Unchanged(t *testing.T) {
	g := New(nil, "")
	bigErr := strings.Repeat("x", b3Threshold+1)
	res := g.Apply(context.Background(), "exec", nil,
		tools.Result{Error: bigErr},
		"")
	// No cache dir → degraded gracefully, error unchanged.
	if res.Error != bigErr {
		t.Fatalf("no cache dir: expected unchanged error, got len=%d", len(res.Error))
	}
}

// --- Nil governor ---

func TestNilGovernor_Passthrough(t *testing.T) {
	var g *Governor
	res := g.Apply(context.Background(), "read", nil,
		tools.Result{Content: "hello"},
		"world")
	if res.Content != "hello" {
		t.Fatalf("nil governor should pass through, got %q", res.Content)
	}
}
