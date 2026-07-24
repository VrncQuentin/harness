// Package tools provides the tool registry and built-in read-only file
// tools for the native agent loop.
package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/vrnc/harness/internal/parser"
)

// ErrSandboxViolation is returned when a file tool tries to access a path
// outside the active project's configured sandbox roots.
var ErrSandboxViolation = errors.New("tools: path is outside sandbox roots")

// ErrDuplicateTool is returned when registering a tool ID more than once.
var ErrDuplicateTool = errors.New("tools: duplicate id")

// ErrPathNotFound is returned when a requested file or directory does not
// exist.
var ErrPathNotFound = errors.New("tools: path not found")

// CallInfo provides the active project context available to every tool call.
// CallerIdentity records who or what requested the tool (e.g. "agent:coder",
// "api", "pipeline:deploy"). SessionID pins the call to the owning session
// for audit trails and episode recording.
type CallInfo struct {
	ProjectSlug     string
	SandboxRoots    []string
	MemoryRepoPaths []string // C2: paths of all project memory repos; git_* write tools reject calls resolving here
	SessionID       string
	CallerIdentity  string
	HTTPClient      *http.Client
}

// OriginClass records where content came from, per the C3 contract:
// parser-backed tool output is extraction-class by construction, while
// model-generated content is inference-class. The tool layer records the
// class so downstream consumers (session records, the future memory layer)
// never have to guess.
type OriginClass string

const (
	// OriginExtraction marks deterministic, parser-backed output.
	OriginExtraction OriginClass = "extraction"
	// OriginInference marks model-generated content.
	OriginInference OriginClass = "inference"
)

// Result is the outcome of a tool execution. Error is set for tool-level
// failures (missing file, sandbox violation); Content is the successful
// output. Both are injected into the conversation for the model to see.
// Origin is set by tools whose output has a provenance class by
// construction; empty means unclassified.
type Result struct {
	Content string
	Error   string
	Origin  OriginClass
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
	{ID: "read", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "file_list", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "ast_map", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "ast_find", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "git_status", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "git_diff", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "git_log", DefaultEnabled: true, DefaultApproval: ApprovalDefaultAllow, DefaultApprovalSource: "builtin: read-only tools allowed"},
	{ID: "edit", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: edits require approval"},
	{ID: "exec", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: exec commands require approval"},
	{ID: "go_test", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: go_test runs the test suite"},
	{ID: "go_lint", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: go_lint runs the linter"},
	{ID: "git_commit", DefaultEnabled: false, DefaultApproval: ApprovalDefaultAsk, DefaultApprovalSource: "builtin: git_commit writes to the repo"},
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
	// caller) under the given call info. Cancellation is propagated via ctx.
	Execute(ctx context.Context, call CallInfo, args map[string]any) Result
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

// RegisterBuiltins registers the built-in tools on r. Read-only tools
// (read, file_list, ast_map, ast_find) are always registered. Destructive tools
// (edit, exec) are registered but disabled by default in
// config — they must be explicitly enabled and pass the approval
// layer before they can execute.
func RegisterBuiltins(r *Registry) error {
	parsers, err := parser.NewRegistry(parser.NewGoFrontEnd())
	if err != nil {
		return fmt.Errorf("tools: parser front-ends: %w", err)
	}
	builtins := map[string]Tool{
		"read":       &readTool{},
		"file_list":  &fileListTool{},
		"ast_map":    &astMapTool{parsers: parsers},
		"ast_find":   &astFindTool{parsers: parsers},
		"git_status": &gitStatusTool{},
		"git_diff":   &gitDiffTool{},
		"git_log":    &gitLogTool{},
		"edit":       &editTool{parsers: parsers},
		"exec":       &execTool{},
		"go_test":    &goTestTool{},
		"go_lint":    &goLintTool{},
		"git_commit": &gitCommitTool{},
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
