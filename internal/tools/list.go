package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// fileListTool implements the file_list tool.
type fileListTool struct{}

var _ Tool = (*fileListTool)(nil)

func (t *fileListTool) ID() string { return "file_list" }
func (t *fileListTool) Description() string {
	return "List files and directories in a given path. Returns a list of entry names."
}
func (t *fileListTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Absolute or relative path to the directory to list",
			},
		},
		"required": []string{"path"},
	}
}

func (t *fileListTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "file_list: missing or invalid path argument"}
	}
	absPath, err := validatePath(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	entries, err := os.ReadDir(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Error: ErrPathNotFound.Error() + ": " + absPath}
		}
		return Result{Error: fmt.Sprintf("file_list: %v", err)}
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return Result{Content: strings.Join(names, "\n")}
}
