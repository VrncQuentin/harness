package agentloop

import (
	"context"
	"errors"
	"testing"
	"time"

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

func newTestEngine(t *testing.T, loopCfg config.LoopConfig) *Engine {
	t.Helper()
	reg := tools.NewRegistry()
	if err := tools.RegisterBuiltins(reg); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	return NewEngine(&mockInferClient{}, reg, loopCfg, tools.Context{}).WithApprovals(
		approvals.NewEvaluator(approvals.DefaultLayer()),
	)
}

func TestRejectDoesNotAddSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(t, cfg)

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
	engine := newTestEngine(t, cfg)

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
	engine := newTestEngine(t, cfg)

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
	engine := newTestEngine(t, cfg)

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
	engine := newTestEngine(t, cfg)

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
	if err := tools.RegisterBuiltins(reg); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	engine := NewEngine(&mockInferClient{}, reg, cfg, tools.Context{})

	// isToolEnabled checks config toggles.
	if engine.isToolEnabled("file_write") {
		t.Error("file_write should be disabled")
	}
	if engine.isToolEnabled("shell_exec") {
		t.Error("shell_exec should be disabled")
	}
	if engine.isToolEnabled("web_search") {
		t.Error("web_search should be disabled")
	}
	if engine.isToolEnabled("unknown_tool") {
		t.Error("unknown_tool should be disabled")
	}
	if !engine.isToolEnabled("file_read") {
		t.Error("file_read should be enabled")
	}
}

func TestUnknownApprovalID(t *testing.T) {
	engine := newTestEngine(t, config.LoopConfig{})
	err := engine.ApplyApproval("nonexistent", approvals.ApprovalResponse{Decision: approvals.Allowed})
	if err == nil {
		t.Error("expected error for unknown approval ID")
	}
}

func TestApprovalWaitTimesOut(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(t, cfg).WithApprovalTimeout(20 * time.Millisecond)
	engine.infer = &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}

	evch := make(chan Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "write file"}}, evch)
	}()

	var approvalID string
	for ev := range evch {
		if ev.Type == EvtApprovalNeeded {
			approvalID = ev.ApprovalID
		}
	}
	if approvalID == "" {
		t.Fatal("expected approval_needed event before timeout")
	}

	err := <-done
	if !errors.Is(err, ErrApprovalTimeout) {
		t.Fatalf("Run error = %v, want ErrApprovalTimeout", err)
	}
	if err := engine.ApplyApproval(approvalID, approvals.ApprovalResponse{Decision: approvals.Allowed}); err == nil {
		t.Fatal("expected timed-out approval id to be removed")
	}
}

func TestStateEventWaitsForDeliveryWhenChannelIsFull(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3}
	engine := newTestEngine(t, cfg)
	engine.infer = &mockInferClient{tokens: []inference.Token{{Content: "dropped text"}, {Done: true}}}

	evch := make(chan Event, 1)
	evch <- Event{Type: EvtText, Content: "existing"}
	done := make(chan error, 1)
	go func() {
		done <- engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "hello"}}, evch)
	}()

	select {
	case err := <-done:
		t.Fatalf("Run finished before the final event could be delivered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if ev := <-evch; ev.Content != "existing" {
		t.Fatalf("first event = %+v, want prefilled event", ev)
	}

	select {
	case ev := <-evch:
		if ev.Type != EvtDone {
			t.Fatalf("event type = %q, want %q", ev.Type, EvtDone)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final event")
	}

	if err := <-done; err != nil {
		t.Fatalf("Run error = %v", err)
	}
}
func TestApprovalNeededDeliveryTimesOutWhenEventChannelIsFull(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(t, cfg).WithApprovalTimeout(20 * time.Millisecond)
	engine.infer = &mockInferClient{tokens: toolCallTokens("file_write", `{"path":"/tmp/test.txt"}`)}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	evch := make(chan Event, 1)
	err := engine.Run(ctx, []inference.Message{{Role: "user", Content: "write file"}}, evch)
	if !errors.Is(err, ErrApprovalTimeout) {
		t.Fatalf("Run error = %v, want ErrApprovalTimeout", err)
	}
}
func TestApprovalNeededEventHasCorrectFields(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, FileWriteEnabled: true}
	engine := newTestEngine(t, cfg)

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
	engine := newTestEngine(t, cfg)

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
		if ev.Type == EvtApproval && ev.ToolError == "denied" {
			foundApproval = true
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

type fakeLoopMetrics struct {
	turns      int
	calls      map[string]int
	errors     map[string]int
	errorRates map[string][]bool
}

func (f *fakeLoopMetrics) LoopTurn() error {
	f.turns++
	return nil
}

func (f *fakeLoopMetrics) ToolCall(tool string) error {
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[tool]++
	return nil
}

func (f *fakeLoopMetrics) ToolCallError(tool string) error {
	if f.errors == nil {
		f.errors = make(map[string]int)
	}
	f.errors[tool]++
	return nil
}

func (f *fakeLoopMetrics) ToolCallErrorRate(tool string, failed bool) error {
	if f.errorRates == nil {
		f.errorRates = make(map[string][]bool)
	}
	f.errorRates[tool] = append(f.errorRates[tool], failed)
	return nil
}

func TestEngineRecordsLoopAndToolMetrics(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3}
	engine := newTestEngine(t, cfg)
	engine.infer = &mockInferClient{tokens: toolCallTokens("file_list", `{"path":"."}`)}
	metrics := &fakeLoopMetrics{}
	engine.WithMetrics(metrics)

	evch := make(chan Event, 64)
	_ = engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "list"}}, evch)
	for range evch {
	}

	if metrics.turns != 2 {
		t.Fatalf("loop turns = %d, want 2", metrics.turns)
	}
	if metrics.calls["file_list"] != 1 {
		t.Fatalf("file_list calls = %d, want 1", metrics.calls["file_list"])
	}
	if metrics.errors["file_list"] != 1 {
		t.Fatalf("file_list errors = %d, want 1", metrics.errors["file_list"])
	}
	if got := metrics.errorRates["file_list"]; len(got) != 1 || !got[0] {
		t.Fatalf("file_list error-rate samples = %v, want [true]", got)
	}
}
