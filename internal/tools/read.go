package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// readTool implements the read tool: range- and locator-addressed reads.
// It returns raw bytes; skeletonization is applied downstream by the
// governor (B1), never here.
type readTool struct{}

var _ Tool = (*readTool)(nil)

func (t *readTool) ID() string { return "read" }

func (t *readTool) Description() string {
	return "Read a file or a line range. Address by path (whole file), path plus start_line/end_line, or a locator (path:start-end) from ast_map/ast_find. Returns raw file content."
}

func (t *readTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to read. Omit when locator is given.",
			},
			"locator": map[string]any{
				"type":        "string",
				"description": "Stable locator (path:start-end) from ast_map/ast_find. Takes precedence over path.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "First line to read (1-based, inclusive). Requires path.",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "Last line to read (1-based, inclusive). Requires path and start_line.",
			},
		},
	}
}

func (t *readTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	target, start, end, err := readTarget(args)
	if err != nil {
		return Result{Error: "read: " + err.Error()}
	}
	absPath, err := validatePath(target, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	//nolint:gosec
	data, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + absPath}
		}
		return Result{Error: fmt.Sprintf("read: %v", err)}
	}
	if start == 0 {
		return Result{Content: string(data)}
	}
	span, err := spanBytes(data, start, end)
	if err != nil {
		return Result{Error: "read: " + err.Error()}
	}
	return Result{Content: string(span)}
}

// readTarget resolves the addressed file and optional range from the
// arguments. start of 0 means "whole file".
func readTarget(args map[string]any) (path string, start, end int, err error) {
	if locator, ok := args["locator"].(string); ok && locator != "" {
		return ParseLocator(locator)
	}
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", 0, 0, errors.New("missing path or locator argument")
	}
	start = intArg(args, "start_line")
	end = intArg(args, "end_line")
	if start == 0 && end == 0 {
		return path, 0, 0, nil
	}
	if start < 1 || end < start {
		return "", 0, 0, fmt.Errorf("invalid line range %d-%d", start, end)
	}
	return path, start, end, nil
}

// intArg reads an integer argument that arrives as a JSON number.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}
