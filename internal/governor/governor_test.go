package governor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VrncQuentin/harness/internal/parser"
	"github.com/VrncQuentin/harness/internal/tools"
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
	// line one – unrelated work
	// line two – unrelated work
	// line three – unrelated work
	// line four – unrelated work
	result := y + "!"
	result = result + " suffix"
	return result
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

// --- B2 tests ---

func TestB2_UnknownTool_Passthrough(t *testing.T) {
	g := New(nil, "")
	big := strings.Repeat("x", 32*1024)
	res := g.Apply(context.Background(), "read", nil, tools.Result{Content: big}, "")
	if res.Content != big {
		t.Fatalf("B2 should not fire for read tool, got len=%d", len(res.Content))
	}
}

func TestB2_UnderLimit_Passthrough(t *testing.T) {
	g := New(nil, "")
	small := strings.Repeat("x", 512)
	res := g.Apply(context.Background(), "exec", nil, tools.Result{Content: small}, "")
	if res.Content != small {
		t.Fatalf("B2 should not fire under limit, got %q", res.Content)
	}
}

func TestB2_LargeContent_Elided(t *testing.T) {
	g := New(nil, "")
	big := strings.Repeat("a", b2Head) + strings.Repeat("b", 10*1024) + strings.Repeat("c", b2Tail)
	res := g.Apply(context.Background(), "exec", nil, tools.Result{Content: big}, "")
	if len(res.Content) >= len(big) {
		t.Fatalf("B2 should have elided content: before=%d after=%d", len(big), len(res.Content))
	}
	if !strings.Contains(res.Content, "bytes elided") {
		t.Fatalf("expected elision marker, got %q", res.Content[:min(200, len(res.Content))])
	}
	// Head must be preserved.
	if !strings.HasPrefix(res.Content, strings.Repeat("a", b2Head)) {
		t.Fatalf("head not preserved")
	}
	// Tail must be preserved.
	if !strings.HasSuffix(res.Content, strings.Repeat("c", b2Tail)) {
		t.Fatalf("tail not preserved")
	}
}

func TestB2_GoLint_Elided(t *testing.T) {
	g := New(nil, "")
	big := strings.Repeat("z", 5*1024)
	res := g.Apply(context.Background(), "go_lint", nil, tools.Result{Content: big}, "")
	if len(res.Content) >= len(big) {
		t.Fatalf("B2 should elide go_lint output: before=%d after=%d", len(big), len(res.Content))
	}
}

func TestB2_EmptyContent_Passthrough(t *testing.T) {
	g := New(nil, "")
	res := g.Apply(context.Background(), "exec", nil, tools.Result{Content: ""}, "")
	if res.Content != "" {
		t.Fatalf("B2 should not fire on empty content, got %q", res.Content)
	}
}

// --- B5 tests ---

func TestB5_ReducingTransform_Kept(t *testing.T) {
	g := New(nil, "")
	// B2 should reduce large exec output; B5 must not revert it.
	big := strings.Repeat("x", 20*1024)
	res := g.Apply(context.Background(), "exec", nil, tools.Result{Content: big}, "")
	if len(res.Content) >= len(big) {
		t.Fatalf("B5 should not revert reducing B2 transform: before=%d after=%d", len(big), len(res.Content))
	}
}

func TestB5_IncreasingTransform_Reverted(t *testing.T) {
	// Test wrapB5 directly: a transform that expands content must be auto-reverted.
	g := New(nil, "")
	pre := tools.Result{Content: "short"}
	post := g.wrapB5(pre, func(r tools.Result) tools.Result {
		return tools.Result{Content: strings.Repeat("x", 1000)}
	})
	if post.Content != "short" {
		t.Fatalf("B5 should revert a transform that increases token count, got len=%d", len(post.Content))
	}
}

func TestB5_WithTokenizer_Used(t *testing.T) {
	called := false
	myTokenizer := func(s string) int {
		called = true
		return len(s) // byte count as proxy
	}
	g := New(nil, "").WithTokenizer(myTokenizer)
	g.Apply(context.Background(), "exec", nil, tools.Result{Content: "hello"}, "")
	if !called {
		t.Fatal("WithTokenizer hook was not called by B5")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readSpill returns the contents of the single file B3 wrote under cacheDir.
func readSpill(t *testing.T, cacheDir string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "toolout")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("spill dir holds %d files, want exactly 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(data)
}

// B3 promises the full unfiltered output on disk. The tools bound what they
// inject inline, so spilling Result.Error wrote the same truncated text the
// model had already been shown and labelled it "full output".
func TestB3_SpillsFullOutputNotTheInlineExcerpt(t *testing.T) {
	cacheDir := t.TempDir()
	g := New(nil, cacheDir)

	full := strings.Repeat("F", b3Threshold*4)
	inline := strings.Repeat("F", 512) + "\n… (truncated)"

	res := g.Apply(context.Background(), "exec", nil,
		tools.Result{Error: inline, FullOutput: full}, "")

	if !strings.Contains(res.Error, "toolout:") {
		t.Fatalf("no toolout handle in %q", res.Error)
	}
	spilled := readSpill(t, cacheDir)
	if spilled != full {
		t.Errorf("spilled %d bytes, want the full %d — the excerpt was written instead",
			len(spilled), len(full))
	}
	// The in-memory copy has served its purpose; carrying it onward would push
	// megabytes into events and session records.
	if res.FullOutput != "" {
		t.Errorf("FullOutput still set after the spill (%d bytes)", len(res.FullOutput))
	}
}

// A small inline error with a large preserved output must still spill: the
// decision is about how much output exists, not how much of it was shown.
func TestB3_SpillsWhenOnlyFullOutputIsLarge(t *testing.T) {
	cacheDir := t.TempDir()
	g := New(nil, cacheDir)

	full := strings.Repeat("G", b3Threshold*2)
	res := g.Apply(context.Background(), "go_test", nil,
		tools.Result{Error: "go_test: exit status 1\n--- FAIL: TestX", FullOutput: full}, "")

	if !strings.Contains(res.Error, "toolout:") {
		t.Fatalf("no toolout handle for a large preserved output: %q", res.Error)
	}
	if spilled := readSpill(t, cacheDir); spilled != full {
		t.Errorf("spilled %d bytes, want %d", len(spilled), len(full))
	}
}

func TestB3_TruncatesOnRuneBoundary(t *testing.T) {
	g := New(nil, t.TempDir())
	// Multi-byte runes across the 512-byte prefix cut.
	res := g.Apply(context.Background(), "exec", nil,
		tools.Result{Error: strings.Repeat("é", b3Threshold)}, "")
	if !utf8.ValidString(res.Error) {
		t.Error("B3 prefix cut produced invalid UTF-8")
	}
}
