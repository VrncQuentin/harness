// Package governor applies result transforms between tool execution and
// context injection in the agent loop. These transforms are not model-callable
// and never reach the agent as tools. Current transforms:
//
//   - B1: query-aware skeletonizer — reduces read output for parser-supported
//     files, keeping full bodies for spans relevant to the active task and
//     emitting only signatures for the rest.
//
//   - B3: tee-on-failure — spills large error outputs to disk and injects a
//     compact handle into the conversation so the model can reference them.
package governor

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/vrnc/harness/internal/parser"
	"github.com/vrnc/harness/internal/tools"
)

// Governor applies governor-side transforms (B1, B3) to raw tool results.
// Construct with New; zero value is safe and acts as a no-op.
type Governor struct {
	parsers  *parser.Registry
	cacheDir string // ~/.harness/cache/toolout
}

// New creates a Governor. parsers may be nil (disables B1). cacheDir is the
// directory for B3 tee files; it is created lazily on first write.
func New(parsers *parser.Registry, cacheDir string) *Governor {
	return &Governor{parsers: parsers, cacheDir: cacheDir}
}

// Apply runs all applicable transforms on res, in order B1 → B3.
// toolID identifies the tool that produced res; args are its call arguments.
// query is the active task prompt, used by B1 for relevance scoring.
func (g *Governor) Apply(ctx context.Context, toolID string, args map[string]any, res tools.Result, query string) tools.Result {
	if g == nil {
		return res
	}
	res = g.applyB1(toolID, args, res, query)
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
