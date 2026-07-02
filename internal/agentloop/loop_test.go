package agentloop

import (
	"context"
	"testing"

	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/tools"
)

// mockInferClient sends a fixed sequence of tokens then closes.
type mockInferClient struct {
	tokens []inference.Token
}

func (m *mockInferClient) Complete(ctx context.Context, req inference.CompletionRequest) (<-chan inference.Token, error) {
	ch := make(chan inference.Token, len(m.tokens)+1)
	go func() {
		for _, tok := range m.tokens {
			select {
			case ch <- tok:
			case <-ctx.Done():
				close(ch)
				return
			}
		}
		close(ch)
	}()
	return ch, nil
}

func (m *mockInferClient) Health(ctx context.Context) error { return nil }

// toolCallTokens returns tokens that simulate a model calling a tool.
func toolCallTokens(toolName, args string) []inference.Token {
	return []inference.Token{
		{
			ToolCallDelta: &inference.ToolCallDelta{
				Index: 0,
				ID:    "call_1",
				Name:  toolName,
			},
		},
		{
			ToolCallDelta: &inference.ToolCallDelta{
				Index:     0,
				Arguments: args,
			},
		},
		{Done: true},
	}
}

func newTestEngine(loopCfg config.LoopConfig) *Engine {
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	return NewEngine(&mockInferClient{}, reg, loopCfg, tools.Context{}).WithApprovals(
		approvals.NewEvaluator(approvals.DefaultLayer()),
	)
}

func TestRejectDoesNotAddSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "write test.txt"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	// Wait for approval-needed event.
	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
			break
		}
	}
	if approvalID == "" {
		t.Fatal("expected approval-needed event")
	}

	// Reject the call.
	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{
		Decision: approvals.Denied,
		Remember: false,
	}); err != nil {
		t.Fatalf("ApplyApproval: %v", err)
	}

	// Drain remaining events.
	for range evch {
	}

	// Verify no session rule was added.
	dec, _ := engine.evl.Evaluate("file_write", "")
	if dec != approvals.Ask {
		t.Errorf("reject should not add session rule; file_write should still Ask, got %s", dec)
	}
}

func TestAllowDoesNotAddSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "write test.txt"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
			break
		}
	}
	if approvalID == "" {
		t.Fatal("expected approval-needed event")
	}

	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{
		Decision: approvals.Allowed,
		Remember: false,
	}); err != nil {
		t.Fatalf("ApplyApproval: %v", err)
	}

	for range evch {
	}

	// Verify no session rule was added.
	dec, _ := engine.evl.Evaluate("file_write", "")
	if dec != approvals.Ask {
		t.Errorf("allow should not add session rule; file_write should still Ask, got %s", dec)
	}
}

func TestAlwaysAddsSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "write test.txt"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
			break
		}
	}
	if approvalID == "" {
		t.Fatal("expected approval-needed event")
	}

	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{
		Decision: approvals.Allowed,
		Remember: true,
	}); err != nil {
		t.Fatalf("ApplyApproval: %v", err)
	}

	for range evch {
	}

	// Verify a session rule WAS added.
	dec, _ := engine.evl.Evaluate("file_write", "")
	if dec != approvals.Allowed {
		t.Errorf("always should add session rule; file_write should be Allowed, got %s", dec)
	}
}

func TestAlwaysForGitStatusDoesNotAllowGitPush(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, ShellExecEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("shell_exec", `{"command":"git status"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "check git"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
			break
		}
	}
	if approvalID == "" {
		t.Fatal("expected approval-needed event for git status")
	}

	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{
		Decision: approvals.Allowed,
		Remember: true,
	}); err != nil {
		t.Fatalf("ApplyApproval: %v", err)
	}

	for range evch {
	}

	// git status (safe) should be allowed by the session rule.
	dec, _ := engine.evl.Evaluate("shell_exec", "git status")
	if dec != approvals.Allowed {
		t.Errorf("git status should be allowed by exact session match, got %s", dec)
	}

	// git push (destructive) should NOT be allowed — classified as destructive
	// and requires an exact match, which doesn't exist.
	dec, _ = engine.evl.Evaluate("shell_exec", "git push origin main")
	if dec != approvals.Ask {
		t.Errorf("git push should still Ask (destructive, no exact match), got %s", dec)
	}
}

func TestDestructiveShellCmdRequiresApproval(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, ShellExecEnabled: true}
	engine := newTestEngine(cfg)

	// Even with a broad shell_exec allow rule, destructive commands should Ask.
	userLayer := approvals.Layer{
		Name: "user-config",
		Rules: []approvals.Rule{
			{ToolID: "shell_exec", Decision: approvals.Allowed, Source: "user: shell allowed"},
		},
	}
	engine.evl = approvals.NewEvaluator(approvals.DefaultLayer(), userLayer)

	// rm is destructive → must Ask even with broad allow.
	dec, _ := engine.evl.Evaluate("shell_exec", "rm -rf /tmp/test")
	if dec != approvals.Ask {
		t.Errorf("rm -rf should Ask even with broad shell allow, got %s", dec)
	}

	// ls is safe → allowed by broad rule.
	dec, _ = engine.evl.Evaluate("shell_exec", "ls")
	if dec != approvals.Allowed {
		t.Errorf("ls should be Allowed by broad rule, got %s", dec)
	}
}

func TestToolDisabledInConfigReturnsNotAvailable(t *testing.T) {
	cfg := config.LoopConfig{
		MaxTurns:         2,
		DoomThreshold:    3,
		FileReadEnabled:  true,
		FileListEnabled:  true,
		FileWriteEnabled: false,
		ShellExecEnabled: false,
	}
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	engine := NewEngine(&mockInferClient{}, reg, cfg, tools.Context{})

	// isToolEnabled checks config toggles.
	if engine.isToolEnabled("file_write") {
		t.Error("file_write should be disabled")
	}
	if engine.isToolEnabled("shell_exec") {
		t.Error("shell_exec should be disabled")
	}
	if !engine.isToolEnabled("file_read") {
		t.Error("file_read should be enabled")
	}
}

func TestUnknownApprovalID(t *testing.T) {
	engine := newTestEngine(config.LoopConfig{})
	err := engine.ApplyApproval("nonexistent", approvals.ApprovalResponse{Decision: approvals.Allowed})
	if err == nil {
		t.Error("expected error for unknown approval ID")
	}
}

func TestApprovalNeededEventHasCorrectFields(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "write file"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	var ev Event
	for ev = range evch {
		if ev.Type == EvtApprovalNeeded {
			break
		}
	}

	if ev.Type != EvtApprovalNeeded {
		t.Fatal("expected approval_needed event")
	}
	if ev.ToolID != "file_write" {
		t.Errorf("expected tool_id file_write, got %s", ev.ToolID)
	}
	if ev.ApprovalID == "" {
		t.Error("approval_id should not be empty")
	}
	if ev.ToolArgs == "" {
		t.Error("tool_args should not be empty")
	}
	if ev.ApprovalReason == "" {
		t.Error("approval_reason should not be empty")
	}

	// Cleanup.
	_ = engine.ApplyApproval(ev.ApprovalID, approvals.ApprovalResponse{
		Decision: approvals.Denied,
		Remember: false,
	})
	for range evch {
	}
}

func TestApprovalDeniedInjectsError(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(cfg)

	client := &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}
	engine.infer = client

	evch := make(chan Event, 64)
	ctx := context.Background()
	msgs := []inference.Message{{Role: "user", Content: "write file"}}

	go func() {
		_ = engine.Run(ctx, msgs, evch)
	}()

	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
			break
		}
	}

	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{
		Decision: approvals.Denied,
		Remember: false,
	}); err != nil {
		t.Fatalf("ApplyApproval: %v", err)
	}

	// Check that a denied approval event and a tool_result with error appear.
	var foundApproval, foundError bool
	for ev := range evch {
		if ev.Type == EvtApproval && ev.ToolError == approvals.Denied.String() {
			foundApproval = true
			if ev.ApprovalDecision != approvals.Denied.String() {
				t.Errorf("approval decision = %q, want %q", ev.ApprovalDecision, approvals.Denied.String())
			}
			if ev.ApprovalScope != approvals.ApprovalScopeOnce {
				t.Errorf("approval scope = %q, want %s", ev.ApprovalScope, approvals.ApprovalScopeOnce)
			}
			if ev.ApprovalReason == "" {
				t.Error("approval reason should not be empty")
			}
		}
		if ev.Type == EvtToolResult && ev.ToolError != "" {
			foundError = true
		}
	}
	if !foundApproval {
		t.Error("expected approval denied event")
	}
	if !foundError {
		t.Error("expected tool result with denial error")
	}
}
