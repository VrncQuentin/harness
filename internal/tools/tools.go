// Package tools provides the tool registry and built-in read-only file
// tools for the native agent loop.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrSandboxViolation is returned when a file tool tries to access a path
// outside the active project's configured sandbox roots.
var ErrSandboxViolation = errors.New("tools: path is outside sandbox roots")

// ErrDuplicateTool is returned when registering a tool ID more than once.
var ErrDuplicateTool = errors.New("tools: duplicate id")

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
	HTTPClient     *http.Client
}

// Result is the outcome of a tool execution. Error is set for tool-level
// failures (missing file, sandbox violation); Content is the successful
// output. Both are injected into the conversation for the model to see.
type Result struct {
	Content string
	Error   string
}

// ApprovalDefault is the built-in approval-layer posture for a tool.
type ApprovalDefault string

const (
	ApprovalDefaultAllow ApprovalDefault = "allow"
	ApprovalDefaultAsk   ApprovalDefault = "ask"
)

// Descriptor is stable metadata for a registered tool. It is the single source
// for built-in enablement and approval defaults; callers derive config and
// policy behavior from these descriptors instead of duplicating tool IDs.
type Descriptor struct {
	ID                    string
	DefaultEnabled        bool
	DefaultApproval       ApprovalDefault
	DefaultApprovalSource string
}

var builtinToolDescriptors = []Descriptor{
	{ID: "file_read", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "file_list", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "file_write", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: writes require approval"},
	{ID: "shell_exec", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: shell commands require approval"},
	{ID: "web_search", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: web search uses the network"},
}

// BuiltinDescriptors returns the built-in tool descriptors in registration order.
func BuiltinDescriptors() []Descriptor {
	out := make([]Descriptor, len(builtinToolDescriptors))
	copy(out, builtinToolDescriptors)
	return out
}

// BuiltinDescriptor returns the descriptor for a built-in tool id.
func BuiltinDescriptor(id string) (Descriptor, bool) {
	for _, desc := range builtinToolDescriptors {
		if desc.ID == id {
			return desc, true
		}
	}
	return Descriptor{}, false
}

// BuiltinDefaultEnabled reports the config default for a built-in tool id.
func BuiltinDefaultEnabled(id string) bool {
	desc, ok := BuiltinDescriptor(id)
	return ok && desc.DefaultEnabled
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

// Register adds t to the registry.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("tools: nil tool")
	}
	id := t.ID()
	if _, exists := r.tools[id]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateTool, id)
	}
	r.tools[id] = t
	r.order = append(r.order, id)
	return nil
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

// validatePath checks that path (after symlink resolution) is within at
// least one sandbox root. If sandbox roots are empty, all paths are rejected.
func validatePath(path string, roots []string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("tools: cannot resolve path: %w", err)
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			return "", fmt.Errorf("tools: resolve sandbox root %s: %w", root, err)
		}
		resolvedAncestor, err := resolveExistingAncestor(abs)
		if err != nil {
			continue
		}
		if pathWithinRoot(resolvedAncestor, resolvedRoot) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrSandboxViolation, path)
}

// resolveExistingAncestor evaluates symlinks on the deepest path component
// already present on disk. This prevents a lexically in-root missing target
// below a symlink or junction from escaping its sandbox.
func resolveExistingAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		current = parent
	}
}
func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// RegisterBuiltins registers the built-in tools on r. Read-only tools
// (file_read, file_list) are always registered. Destructive tools
// (file_write, shell_exec) are registered but disabled by default in
// config — they must be explicitly enabled and pass the approval
// layer before they can execute.
func RegisterBuiltins(r *Registry) error {
	builtins := map[string]Tool{
		"file_read":  &fileReadTool{},
		"file_list":  &fileListTool{},
		"file_write": &fileWriteTool{},
		"shell_exec": &shellExecTool{},
		"web_search": &webSearchTool{},
	}
	for _, desc := range builtinToolDescriptors {
		t := builtins[desc.ID]
		if t == nil {
			return fmt.Errorf("tools: missing built-in implementation for %q", desc.ID)
		}
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
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

const shellOutputLimit = 64 * 1024

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

	name, shellArgs := shellCommand(cmdStr)
	cmd := exec.CommandContext(timeoutCtx, name, shellArgs...)
	cmd.Dir = workDir
	output := newCappedOutput(shellOutputLimit)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return Result{Error: fmt.Sprintf("shell_exec: %v\n%s", err, output.String())}
	}
	return Result{Content: output.String()}
}

type cappedOutput struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func newCappedOutput(limit int) *cappedOutput {
	return &cappedOutput{limit: limit}
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - len(b.buf)
	if remaining > 0 {
		if len(p) <= remaining {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:remaining]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := string(b.buf)
	if b.truncated {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "... (output truncated)"
	}
	return out
}
func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/d", "/s", "/c", command}
	}
	return "sh", []string{"-c", command}
}

type webSearchTool struct{}

var _ Tool = (*webSearchTool)(nil)

func (t *webSearchTool) ID() string { return "web_search" }
func (t *webSearchTool) Description() string {
	return "Search the web using a network request. Returns short result titles, URLs, and snippets."
}
func (t *webSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query to send over the network",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of results to return, from 1 to 5",
			},
		},
		"required": []string{"query"},
	}
}

func (t *webSearchTool) Execute(ctx context.Context, c Context, args map[string]any) Result {
	query, ok := args["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || query == "" {
		return Result{Error: "web_search: missing or invalid query argument"}
	}
	maxResults := 3
	if raw, ok := args["max_results"].(float64); ok {
		maxResults = min(max(int(raw), 1), 5)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	reqURL := "https://api.duckduckgo.com/?" + url.Values{
		"q":             []string{query},
		"format":        []string{"json"},
		"no_html":       []string{"1"},
		"skip_disambig": []string{"1"},
		"no_redirect":   []string{"1"},
		"t":             []string{"harness"},
		"pretty":        []string{"0"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: build request: %v", err)}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "harness-local/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: network request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{Error: fmt.Sprintf("web_search: network request returned HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: read response: %v", err)}
	}
	results, err := parseDuckDuckGoResults(body, maxResults)
	if err != nil {
		return Result{Error: fmt.Sprintf("web_search: parse response: %v", err)}
	}
	if len(results) == 0 {
		return Result{Content: fmt.Sprintf("Network request used for web_search query %q.\nNo results returned.", query)}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Network request used for web_search query %q.\n", query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n%s\n%s\n", i+1, r.Title, r.URL, r.Snippet)
	}
	return Result{Content: strings.TrimSpace(b.String())}
}

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func parseDuckDuckGoResults(body []byte, maxResults int) ([]searchResult, error) {
	var payload struct {
		AbstractText   string `json:"AbstractText"`
		AbstractSource string `json:"AbstractSource"`
		AbstractURL    string `json:"AbstractURL"`
		Heading        string `json:"Heading"`
		RelatedTopics  []struct {
			FirstURL string `json:"FirstURL"`
			Text     string `json:"Text"`
			Topics   []struct {
				FirstURL string `json:"FirstURL"`
				Text     string `json:"Text"`
			} `json:"Topics"`
		} `json:"RelatedTopics"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	results := make([]searchResult, 0, maxResults)
	if payload.AbstractText != "" || payload.AbstractURL != "" {
		title := payload.Heading
		if title == "" {
			title = payload.AbstractSource
		}
		addSearchResult(&results, maxResults, title, payload.AbstractURL, payload.AbstractText)
	}
	for _, topic := range payload.RelatedTopics {
		addSearchResult(&results, maxResults, topicTitle(topic.Text), topic.FirstURL, topic.Text)
		for _, child := range topic.Topics {
			addSearchResult(&results, maxResults, topicTitle(child.Text), child.FirstURL, child.Text)
		}
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

func addSearchResult(results *[]searchResult, maxResults int, title, resultURL, snippet string) {
	if len(*results) >= maxResults || (strings.TrimSpace(title) == "" && strings.TrimSpace(snippet) == "") {
		return
	}
	*results = append(*results, searchResult{
		Title:   fallback(strings.TrimSpace(title), "Untitled result"),
		URL:     strings.TrimSpace(resultURL),
		Snippet: strings.TrimSpace(snippet),
	})
}

func topicTitle(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if i := strings.Index(text, " - "); i > 0 {
		return text[:i]
	}
	if len(text) > 80 {
		return text[:80] + "..."
	}
	return text
}

func fallback(value, def string) string {
	if value == "" {
		return def
	}
	return value
}
