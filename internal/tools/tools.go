// Package tools provides the tool registry and built-in read-only file
// tools for the M4 native agent loop.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrSandboxViolation is returned when a file tool tries to access a path
// outside the active project's configured sandbox roots.
var ErrSandboxViolation = errors.New("tools: path is outside sandbox roots")

// ErrPathNotFound is returned when a requested file or directory does not
// exist.
var ErrPathNotFound = errors.New("tools: path not found")

// Context provides the active project context available to every tool call.
// CallerIdentity records who or what requested the tool (e.g. "agent:coder",
// "api", "pipeline:deploy"). SessionID pins the call to the owning session
// for audit trails and episode recording.
type Context struct {
	ProjectSlug    string
	SandboxRoots   []string
	SessionID      string
	CallerIdentity string
	Ctx            context.Context
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

// validatePath checks that path (after symlink resolution) is within at
// least one sandbox root. If sandbox roots are empty, all paths are rejected.
func validatePath(path string, roots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Path does not exist or cannot be resolved — still validate the
		// unresolved path against sandbox roots to prevent traversal.
		for _, root := range roots {
			clean := filepath.Clean(root)
			if strings.HasPrefix(abs, clean+string(filepath.Separator)) || abs == clean {
				return abs, nil
			}
		}
		return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
	}
	for _, root := range roots {
		clean := filepath.Clean(root)
		resolvedRoot, err := filepath.EvalSymlinks(clean)
		if err != nil {
			resolvedRoot = clean
		}
		if strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) || resolved == resolvedRoot {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
}

// RegisterBuiltins registers the built-in tools on r. Read-only tools
// (file_read, file_list) are always registered. Destructive tools
// (file_write, shell_exec) are registered but disabled by default in
// config — they must be explicitly enabled and pass the M7 approval
// layer before they can execute.
func RegisterBuiltins(r *Registry) {
	r.Register(&fileReadTool{})
	r.Register(&fileListTool{})
	r.Register(&fileWriteTool{})
	r.Register(&shellExecTool{})
}

// fileReadTool implements the file_read tool.
type fileReadTool struct{}

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

func (t *fileWriteTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	rawPath, ok := args["path"].(string)
	if !ok || rawPath == "" {
		return Result{Error: "file_write: missing or invalid path argument"}
	}
	content, _ := args["content"].(string)
	absPath, err := validatePath(rawPath, c.SandboxRoots)
	if err != nil {
		return Result{Error: err.Error()}
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return Result{Error: fmt.Sprintf("file_write: %v", err)}
	}
	return Result{Content: fmt.Sprintf("Wrote %d bytes to %s", len(content), absPath)}
}

type shellExecTool struct{}

var _ Tool = (*shellExecTool)(nil)

func (t *shellExecTool) ID() string { return "shell_exec" }
func (t *shellExecTool) Description() string {
	return "Execute a shell command. The command runs inside the sandbox root directory. Commands are limited to 30s and output is truncated to 64KB."
}
func (t *shellExecTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to execute"},
		},
		"required": []string{"command"},
	}
}

func (t *shellExecTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	cmdStr, ok := args["command"].(string)
	if !ok || cmdStr == "" {
		return Result{Error: "shell_exec: missing or invalid command argument"}
	}
	if len(c.SandboxRoots) == 0 || c.SandboxRoots[0] == "" {
		return Result{Error: "shell_exec: no sandbox root configured — cannot determine working directory"}
	}
	workDir := c.SandboxRoots[0]

	// Validate workDir the same way file tools validate paths.
	if _, err := validatePath(workDir, c.SandboxRoots); err != nil {
		return Result{Error: fmt.Sprintf("shell_exec: invalid working directory: %v", err)}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "sh", "-c", cmdStr)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return Result{Error: fmt.Sprintf("shell_exec: %v\n%s", err, truncateBytes(output, 65536))}
	}
	return Result{Content: truncateBytes(output, 65536)}
}

func truncateBytes(data []byte, max int) string {
	if len(data) > max {
		return string(data[:max]) + "\n... (output truncated)"
	}
	return string(data)
}
