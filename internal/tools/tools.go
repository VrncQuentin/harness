// Package tools provides the tool registry and built-in read-only file
// tools for the M4 native agent loop.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSandboxViolation is returned when a file tool tries to access a path
// outside the active project's configured sandbox roots.
var ErrSandboxViolation = errors.New("tools: path is outside sandbox roots")

// ErrPathNotFound is returned when a requested file or directory does not
// exist.
var ErrPathNotFound = errors.New("tools: path not found")

// Context provides the active project context available to every tool call.
type Context struct {
	ProjectSlug  string
	SandboxRoots []string
}

// Result is the outcome of a tool execution. Error is set for tool-level
// failures (missing file, sandbox violation); Content is the successful
// output. Both are injected into the conversation for the model to see.
type Result struct {
	Content string
	Error   string
}

// Tool is a single callable tool registered with the harness.
type Tool interface {
	// ID returns the unique tool identifier, used in tool calls from the
	// model and for enable/disable toggles.
	ID() string
	// Schema returns the JSON Schema for the tool's parameters as a
	// serializable map.
	Schema() map[string]any
	// Execute runs the tool with the given arguments (JSON-decoded by the
	// caller) under the given context. Cancellation is propagated via ctx.
	Execute(ctx context.Context, ctx2 Context, args map[string]any) Result
	// Description returns a short human-readable description for the
	// model's tool choice head.
	Description() string
}

// Registry holds all registered tools and resolves tool calls to execution.
type Registry struct {
	tools map[string]Tool
	order []string // insertion order for list stability
}

// NewRegistry returns an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t to the registry. Panics on duplicate IDs.
func (r *Registry) Register(t Tool) {
	if _, exists := r.tools[t.ID()]; exists {
		panic(fmt.Sprintf("tools: duplicate id %q", t.ID()))
	}
	r.tools[t.ID()] = t
	r.order = append(r.order, t.ID())
}

// Get returns the tool identified by id, or nil if not registered.
func (r *Registry) Get(id string) Tool {
	return r.tools[id]
}

// List returns all registered tools in insertion order.
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.tools[id])
	}
	return out
}

// Schemas returns the inference.Tool definitions for all registered tools.
func (r *Registry) Schemas() []map[string]any {
	out := make([]map[string]any, 0, len(r.order))
	for _, id := range r.order {
		t := r.tools[id]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        id,
				"description": t.Description(),
				"parameters":  t.Schema(),
			},
		})
	}
	return out
}

// validatePath checks that path is within at least one sandbox root. If
// sandbox roots are empty, all paths are rejected.
func validatePath(path string, roots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path: %w", err)
	}
	for _, root := range roots {
		clean := filepath.Clean(root)
		if strings.HasPrefix(abs, clean+string(filepath.Separator)) || abs == clean {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
}

// RegisterBuiltins registers the M4 read-only file tools on r.
func RegisterBuiltins(r *Registry) {
	r.Register(&fileReadTool{})
	r.Register(&fileListTool{})
}

// fileReadTool implements the file_read tool.
type fileReadTool struct{}

func (t *fileReadTool) ID() string          { return "file_read" }
func (t *fileReadTool) Description() string { return "Read the contents of a file. Returns the file content or an error." }
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

// fileListTool implements the file_list tool.
type fileListTool struct{}

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
