// Package governor applies result transforms between tool execution and
// context injection in the agent loop. These transforms are not model-callable
// and never reach the agent as tools. Current transforms:
//
//   - B1: query-aware skeletonizer — reduces read output for parser-supported
//     files, keeping full bodies for spans relevant to the active task and
//     emitting only signatures for the rest.
//
//   - B2: tool-output folder — per-tool content cap with head/tail elision for
//     high-volume toolchain tools (exec, go_test, go_lint, git_diff, git_log).
//
//   - B3: tee-on-failure — spills large error outputs to disk and injects a
//     compact handle into the conversation so the model can reference them.
//
//   - B5: token gate — reverter guard applied after each context-reshaping
//     transform (B1, B2) using the same rune-quarter heuristic as prompt
//     budgeting; auto-reverts any transform that increases the token count.
//     B3 is exempt: it moves output to disk rather than reshaping context, and
//     its write cannot be undone by discarding the returned result.
package governor

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/VrncQuentin/harness/internal/parser"
	"github.com/VrncQuentin/harness/internal/tools"
)

// Governor applies governor-side transforms (B1, B2, B3, B5) to raw tool
// results. Construct with New; zero value is safe and acts as a no-op.
type Governor struct {
	parsers   *parser.Registry
	cacheDir  string             // ~/.harness/cache/toolout
	tokenizer func(s string) int // nil → tokens.Estimate (rune-quarter heuristic)
}

// New creates a Governor. parsers may be nil (disables B1). cacheDir is the
// directory for B3 tee files; it is created lazily on first write.
func New(parsers *parser.Registry, cacheDir string) *Governor {
	return &Governor{parsers: parsers, cacheDir: cacheDir}
}

// WithTokenizer sets an alternative token-counting function used by B5. The
// default is the rune-quarter heuristic (tokens.Estimate). Callers swap in a
// real tokenizer here without changing the B5 logic — both sides of the
// before/after comparison always use the same counter.
func (g *Governor) WithTokenizer(fn func(s string) int) *Governor {
	g.tokenizer = fn
	return g
}

// Apply runs all applicable transforms on res, in order B1 → B2 → B3.
// B1 and B2 are wrapped by the B5 token gate, which auto-reverts either when it
// increases the estimated token count. B3 is exempt — see the comment at its
// call site below.
// toolID identifies the tool that produced res; args are its call arguments.
// query is the active task prompt, used by B1 for relevance scoring.
func (g *Governor) Apply(ctx context.Context, toolID string, args map[string]any, res tools.Result, query string) tools.Result {
	if g == nil {
		return res
	}
	res = g.wrapB5(res, func(r tools.Result) tools.Result { return g.applyB1(toolID, args, r, query) })
	res = g.wrapB5(res, func(r tools.Result) tools.Result { return g.applyB2(toolID, r) })
	// B3 is deliberately outside the token gate.
	//
	// B1 and B2 reshape context, so reverting one when it grew the count is
	// exactly right. B3 does not reshape context: it writes the output to disk
	// and adds a fixed handle of a few dozen bytes. Gating it broke in two ways.
	// The handle can be longer than the inline text it is appended to — a short
	// failure with a large preserved output is the normal case now — so the
	// gate reverted precisely the spills worth keeping. And the revert is not a
	// revert: applyB3 has already written the file, so reverting leaves the
	// spill orphaned on disk with nothing in context pointing at it.
	res = g.applyB3(ctx, toolID, res)
	return res
}

// tooloutDir returns the B3 spill directory, creating it if needed.
func (g *Governor) tooloutDir() string {
	if g.cacheDir == "" {
		return ""
	}
	dir := filepath.Join(g.cacheDir, "toolout")
	// best-effort; B3 degrades gracefully if this fails
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// activeQueryTokens splits a query string into lowercase, alpha-only tokens
// of three or more characters. Used by B1 to score symbol relevance.
func activeQueryTokens(query string) []string {
	var tokens []string
	for _, word := range strings.FieldsFunc(query, func(r rune) bool {
		return ('a' > r || r > 'z') && ('A' > r || r > 'Z') && ('0' > r || r > '9')
	}) {
		if len(word) >= 3 {
			tokens = append(tokens, strings.ToLower(word))
		}
	}
	return tokens
}
