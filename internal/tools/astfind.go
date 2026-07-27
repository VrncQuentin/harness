package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/VrncQuentin/harness/internal/parser"
)

// astFindMaxMatches caps content-mode matches per call so a common substring
// cannot flood the context.
const astFindMaxMatches = 20

// astFindTool implements the ast_find tool: symbol- and content-anchored
// locate. Every match carries a stable locator and a content hash — the only
// valid anchor input for edit — and never a bare line number.
type astFindTool struct {
	parsers *parser.Registry
}

var _ Tool = (*astFindTool)(nil)

func (t *astFindTool) ID() string { return "ast_find" }

func (t *astFindTool) Description() string {
	return fmt.Sprintf(
		"Locate a symbol or exact text in a file. Returns stable locators (path:start-end) with content hashes required by edit. Symbol mode is parser-backed (languages: %s); content mode matches literal text in any file.",
		strings.Join(t.parsers.Languages(), ", "),
	)
}

func (t *astFindTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to search",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Symbol name (symbol mode) or exact single-line text (content mode)",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{"symbol", "content"},
				"description": "symbol: parser-backed declaration lookup; content: literal text match on any file. Default symbol.",
			},
		},
		"required": []string{"path", "query"},
	}
}

func (t *astFindTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "ast_find: missing or invalid path argument"}
	}
	query, ok := args["query"].(string)
	if !ok || query == "" {
		return Result{Error: "ast_find: missing or invalid query argument"}
	}
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "symbol"
	}
	file, err := openTarget(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	defer file.Close() //nolint:errcheck // read-only root handle
	absPath := file.Display()
	src, err := file.Read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + absPath}
		}
		return Result{Error: fmt.Sprintf("ast_find: %v", err)}
	}

	switch mode {
	case "symbol":
		return t.findSymbol(absPath, src, query)
	case "content":
		return t.findContent(absPath, src, query)
	default:
		return Result{Error: fmt.Sprintf("ast_find: invalid mode %q — use symbol or content", mode)}
	}
}

func (t *astFindTool) findSymbol(path string, src []byte, query string) Result {
	front, ok := t.parsers.ForPath(path)
	if !ok {
		return Result{Error: fmt.Sprintf(
			"ast_find: symbol mode does not support %q (languages: %s) — use content mode",
			path, strings.Join(t.parsers.Languages(), ", "))}
	}
	symbols, err := front.Outline(src)
	if err != nil {
		return Result{Error: fmt.Sprintf("ast_find: %v", err)}
	}
	matches := matchSymbols(symbols, query)
	if len(matches) == 0 {
		return Result{
			Content: fmt.Sprintf("no symbol matching %q — run ast_map for the full outline", query),
			Origin:  OriginExtraction,
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d symbol match(es) for %q\n", len(matches), query)
	for _, sym := range matches {
		line, err := formatSymbolLine(path, src, sym)
		if err != nil {
			return Result{Error: fmt.Sprintf("ast_find: %v", err)}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return Result{Content: strings.TrimRight(b.String(), "\n"), Origin: OriginExtraction}
}

// matchSymbols prefers exact name matches (including Receiver.Name form) and
// falls back to case-insensitive substring matches.
func matchSymbols(symbols []parser.Symbol, query string) []parser.Symbol {
	var exact, fuzzy []parser.Symbol
	lowered := strings.ToLower(query)
	for _, sym := range symbols {
		qualified := sym.Name
		if sym.Receiver != "" {
			qualified = sym.Receiver + "." + sym.Name
		}
		switch {
		case sym.Name == query || qualified == query:
			exact = append(exact, sym)
		case strings.Contains(strings.ToLower(qualified), lowered):
			fuzzy = append(fuzzy, sym)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fuzzy
}

func (t *astFindTool) findContent(path string, src []byte, query string) Result {
	if strings.ContainsAny(query, "\r\n") {
		return Result{Error: "ast_find: content mode takes a single-line query"}
	}
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	var b strings.Builder
	total := 0
	for i, line := range lines {
		if !strings.Contains(line, query) {
			continue
		}
		total++
		if total > astFindMaxMatches {
			continue
		}
		hash, err := SpanHash(src, i+1, i+1)
		if err != nil {
			return Result{Error: fmt.Sprintf("ast_find: %v", err)}
		}
		fmt.Fprintf(&b, "%s %s %s\n", FormatLocator(path, i+1, i+1), hash, truncateLine(line, 200))
	}
	if total == 0 {
		return Result{Content: fmt.Sprintf("no content matching %q", query), Origin: OriginExtraction}
	}
	out := fmt.Sprintf("%d content match(es) for %q\n%s", total, query, b.String())
	if total > astFindMaxMatches {
		out += fmt.Sprintf("... (%d more matches not shown)", total-astFindMaxMatches)
	}
	return Result{Content: strings.TrimRight(out, "\n"), Origin: OriginExtraction}
}

func truncateLine(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	return line[:limit] + "…"
}
