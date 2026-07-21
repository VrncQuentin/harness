package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// fileReadTool implements the file_read tool.
type fileReadTool struct{}

var _ Tool = (*fileReadTool)(nil)

func (t *fileReadTool) ID() string { return "file_read" }
func (t *fileReadTool) Description() string {
	return "Read the contents of a file. Returns the file content or an error."
}
func (t *fileReadTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute or relative path to the file to read",
			},
		},
		"required": []string{"path"},
	}
}

func (t *fileReadTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "file_read: missing or invalid path argument"}
	}
	absPath, err := validatePath(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	//nolint:gosec
	data, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + absPath}
		}
		return Result{Error: fmt.Sprintf("file_read: %v", err)}
	}
	return Result{Content: string(data)}
}
