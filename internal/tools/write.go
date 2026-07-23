package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type fileWriteTool struct{}

var _ Tool = (*fileWriteTool)(nil)

func (t *fileWriteTool) ID() string { return "file_write" }
func (t *fileWriteTool) Description() string {
	return "Write content to a file. Creates the file if it does not exist, overwrites if it does."
}
func (t *fileWriteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Absolute or relative path to the file to write"},
			"content": map[string]any{"type": "string", "description": "The content to write to the file"},
		},
		"required": []string{"path", "content"},
	}
}

func (t *fileWriteTool) Execute(ctx context.Context, c CallInfo, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "file_write: missing or invalid path argument"}
	}
	content, ok := args["content"].(string)
	if !ok {
		return Result{Error: "file_write: missing or invalid content argument"}
	}
	absPath, err := validatePath(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return Result{Error: fmt.Sprintf("file_write: create parent directories: %v", err)}
	}
	if _, err := validatePath(absPath, c.SandboxRoots); err != nil {
		return Result{Error: err.Error()}
	}
	if err := writeFileAtomic(absPath, []byte(content)); err != nil {
		return Result{Error: fmt.Sprintf("file_write: %v", err)}
	}
	return Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(content), absPath)}
}

func writeFileAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".harness-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
