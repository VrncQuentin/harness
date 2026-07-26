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
//   - B5: token gate — reverter guard applied after each pure transform
//     (B1, B2) using the same rune-quarter heuristic as prompt budgeting;
//     auto-reverts either when it increases the token count.
//     B3 is exempt: it is side-effectful, so discarding its result does not
//     undo it, and its bounded locator must survive or the spilled output
//     becomes unreachable.
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
	// B1 and B2 are pure: they rewrite the result and nothing else, so
	// discarding the rewrite genuinely undoes them. B3 is side-effectful — it
	// has written the spill file by the time the gate inspects its return
	// value — so discarding that value does not undo the transform. It only
	// throws away the locator, leaving the file orphaned on disk with nothing
	// in context pointing at it.
	//
	// B3 does rewrite Error, replacing it with a prefix plus the handle, and
	// that rewrite can grow the text: the handle is often longer than the short
	// inline failure it is appended to now that a small failure can carry a
	// large preserved output. Under the gate that growth reverted exactly the
	// spills worth keeping. Retaining the bounded locator is the requirement —
	// it is the only way back to output that is otherwise unreachable — so the
	// few dozen bytes it costs are not the gate's to reclaim.
	res = g.applyB3(ctx, toolID, res)
	return res
}

// TooloutDir returns the directory B3 spills into for a given cache dir.
//
// Exported so the tool layer can be pointed at the same directory without
// duplicating the path. internal/governor imports internal/tools, so the
// dependency cannot run the other way: the wiring layer calls this and passes
// the result down through CallInfo.
func TooloutDir(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Join(cacheDir, "toolout")
}

// tooloutDir returns the B3 spill directory, creating it if needed.
func (g *Governor) tooloutDir() string {
	dir := TooloutDir(g.cacheDir)
	if dir == "" {
		return ""
	}
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
