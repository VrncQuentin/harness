package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/VrncQuentin/harness/internal/parser"
)

// astMapTool implements the ast_map tool: a deterministic structural outline
// of a single source file, produced by a registered parser front-end.
type astMapTool struct {
	parsers *parser.Registry
}

var _ Tool = (*astMapTool)(nil)

func (t *astMapTool) ID() string { return "ast_map" }

func (t *astMapTool) Description() string {
	return fmt.Sprintf(
		"Structural outline of a source file: every top-level symbol with a stable locator (path:start-end) and content hash. Supported languages: %s.",
		strings.Join(t.parsers.Languages(), ", "),
	)
}

func (t *astMapTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Path to the source file to outline (extensions: %s)", strings.Join(t.parsers.Extensions(), ", ")),
			},
		},
		"required": []string{"path"},
	}
}

func (t *astMapTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "ast_map: missing or invalid path argument"}
	}
	absPath, err := validatePath(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	front, ok := t.parsers.ForPath(absPath)
	if !ok {
		return Result{Error: fmt.Sprintf("ast_map: unsupported file type %q — supported languages: %s", absPath, strings.Join(t.parsers.Languages(), ", "))}
	}
	//nolint:gosec
	src, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + absPath}
		}
		return Result{Error: fmt.Sprintf("ast_map: %v", err)}
	}
	symbols, err := front.Outline(src)
	if err != nil {
		return Result{Error: fmt.Sprintf("ast_map: %v", err)}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s file — %d symbols\n", front.Language(), len(symbols))
	for _, sym := range symbols {
		line, err := formatSymbolLine(absPath, src, sym)
		if err != nil {
			return Result{Error: fmt.Sprintf("ast_map: %v", err)}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return Result{Content: strings.TrimRight(b.String(), "\n"), Origin: OriginExtraction}
}

// formatSymbolLine renders one outline entry:
// "<locator> <hash> [kind] <signature>".
func formatSymbolLine(path string, src []byte, sym parser.Symbol) (string, error) {
	hash, err := SpanHash(src, sym.Span.StartLine, sym.Span.EndLine)
	if err != nil {
		return "", err
	}
	name := sym.Name
	if sym.Receiver != "" {
		name = sym.Receiver + "." + sym.Name
	}
	return fmt.Sprintf("%s %s [%s] %s — %s",
		FormatLocator(path, sym.Span.StartLine, sym.Span.EndLine), hash, sym.Kind, name, sym.Signature), nil
}
