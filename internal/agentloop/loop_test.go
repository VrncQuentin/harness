package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VrncQuentin/harness/internal/approvals"
	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/tools"
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

type countingSchemaTool struct {
	schemaCalls int
}

func (t *countingSchemaTool) ID() string          { return "file_list" }
func (t *countingSchemaTool) Description() string { return "count schema calls" }
func (t *countingSchemaTool) Schema() map[string]any {
	t.schemaCalls++
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (t *countingSchemaTool) Execute(context.Context, tools.CallInfo, map[string]any) tools.Result {
	return tools.Result{Content: "ok"}
}

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
	return NewEngine(&mockInferClient{}, reg, loopCfg, tools.CallInfo{}).WithApprovals(
		approvals.NewEvaluator(approvals.DefaultLayer()),
	)
}

func TestEngineCachesToolSchemasAcrossTurns(t *testing.T) {
	tool := &countingSchemaTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(tool); err != nil {
		t.Fatalf("Register: %v", err)
	}
	engine := NewEngine(
		&mockInferClient{tokens: toolCallTokens("file_list", `{"path":"."}`)},
		reg,
		config.LoopConfig{MaxTurns: 3, DoomThreshold: 10, FileListEnabled: true},
		tools.CallInfo{},
	)
	if tool.schemaCalls != 1 {
		t.Fatalf("NewEngine called Schema() %d times, want 1", tool.schemaCalls)
	}

	evch := make(chan Event, 64)
	err := engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "list"}}, evch)
	if !errors.Is(err, ErrLoopLimitReached) {
		t.Fatalf("Run error = %v, want ErrLoopLimitReached", err)
	}
	if tool.schemaCalls != 1 {
		t.Fatalf("Schema() calls after multi-turn run = %d, want cached 1", tool.schemaCalls)
	}
}

func TestRunRejectsOutOfRangeToolCallIndex(t *testing.T) {
	engine := newTestEngine(t, config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, ReadEnabled: true})
	engine.infer = &mockInferClient{tokens: []inference.Token{
		{ToolCallDelta: &inference.ToolCallDelta{Index: 1_000_000, ID: "call_bad", Name: "read"}},
		{Done: true},
	}}

	evch := make(chan Event, 4)
	err := engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "read"}}, evch)
	if err == nil || !strings.Contains(err.Error(), "outside allowed range") {
		t.Fatalf("Run error = %v, want range error", err)
	}
	var gotErrorEvent bool
	for ev := range evch {
		if ev.Type == EvtError && strings.Contains(ev.Content, "outside allowed range") {
			gotErrorEvent = true
		}
	}
	if !gotErrorEvent {
		t.Fatal("expected error event for out-of-range tool call index")
	}
}

func TestRejectDoesNotAddSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}
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
	dec, _ := engine.evl.Evaluate("edit", "")
	if dec != approvals.Ask {
		t.Errorf("reject should not add session rule; edit should still Ask, got %s", dec)
	}
}

func TestAllowDoesNotAddSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}
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
	dec, _ := engine.evl.Evaluate("edit", "")
	if dec != approvals.Ask {
		t.Errorf("allow should not add session rule; edit should still Ask, got %s", dec)
	}
}

func TestAlwaysAddsSessionRule(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}
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
	dec, _ := engine.evl.Evaluate("edit", "")
	if dec != approvals.Allowed {
		t.Errorf("always should add session rule; edit should be Allowed, got %s", dec)
	}
}

func TestAlwaysForGitStatusDoesNotAllowGitPush(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, ExecEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("exec", `{"cmd":["git","status"]}`)}
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

	// git status should still ask despite the remembered session allow.
	dec, _ := engine.evl.Evaluate("exec", "git status")
	if dec != approvals.Ask {
		t.Errorf("git status should still Ask despite exact session match, got %s", dec)
	}

	// git push (destructive) should NOT be allowed — classified as destructive
	// and requires an exact match, which doesn't exist.
	dec, _ = engine.evl.Evaluate("exec", "git push origin main")
	if dec != approvals.Ask {
		t.Errorf("git push should still Ask (destructive, no exact match), got %s", dec)
	}
}

func TestDestructiveShellCmdRequiresApproval(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, ExecEnabled: true}
	engine := newTestEngine(t, cfg)

	// Even with a broad exec allow rule, shell commands should Ask.
	userLayer := approvals.Layer{
		Name: "user-config",
		Rules: []approvals.Rule{
			{ToolID: "exec", Decision: approvals.Allowed, Source: "user: shell allowed"},
		},
	}
	engine.evl = approvals.NewEvaluator(approvals.DefaultLayer(), userLayer)

	// rm is destructive → must Ask even with broad allow.
	dec, _ := engine.evl.Evaluate("exec", "rm -rf /tmp/test")
	if dec != approvals.Ask {
		t.Errorf("rm -rf should Ask even with broad shell allow, got %s", dec)
	}

	// ls still asks because all shell commands require approval.
	dec, _ = engine.evl.Evaluate("exec", "ls")
	if dec != approvals.Ask {
		t.Errorf("ls should still Ask despite broad shell allow, got %s", dec)
	}
}

func TestToolDisabledInConfigReturnsNotAvailable(t *testing.T) {
	cfg := config.LoopConfig{
		MaxTurns:        2,
		DoomThreshold:   3,
		ReadEnabled:     true,
		FileListEnabled: true,
		EditEnabled:     false,
		ExecEnabled:     false,
	}
	reg := tools.NewRegistry()
	if err := tools.RegisterBuiltins(reg); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	engine := NewEngine(&mockInferClient{}, reg, cfg, tools.CallInfo{})

	// isToolEnabled checks config toggles.
	if engine.isToolEnabled("edit") {
		t.Error("edit should be disabled")
	}
	if engine.isToolEnabled("exec") {
		t.Error("exec should be disabled")
	}
	if engine.isToolEnabled("web_search") {
		t.Error("web_search should be disabled")
	}
	if engine.isToolEnabled("unknown_tool") {
		t.Error("unknown_tool should be disabled")
	}
	if !engine.isToolEnabled("read") {
		t.Error("read should be enabled")
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
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg).WithApprovalTimeout(20 * time.Millisecond)
	engine.infer = &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}

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
	engine.infer = &mockInferClient{tokens: []inference.Token{{Content: "delivered text"}, {Done: true}}}

	evch := make(chan Event, 1)
	evch <- Event{Type: EvtText, Content: "existing"}
	done := make(chan error, 1)
	go func() {
		done <- engine.Run(context.Background(), []inference.Message{{Role: "user", Content: "hello"}}, evch)
	}()

	select {
	case err := <-done:
		t.Fatalf("Run finished before the text event could be delivered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if ev := <-evch; ev.Content != "existing" {
		t.Fatalf("first event = %+v, want prefilled event", ev)
	}

	select {
	case ev := <-evch:
		if ev.Type != EvtText || ev.Content != "delivered text" {
			t.Fatalf("event = %+v, want delivered text event", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text event")
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
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg).WithApprovalTimeout(20 * time.Millisecond)
	engine.infer = &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	evch := make(chan Event, 1)
	err := engine.Run(ctx, []inference.Message{{Role: "user", Content: "write file"}}, evch)
	if !errors.Is(err, ErrApprovalTimeout) {
		t.Fatalf("Run error = %v, want ErrApprovalTimeout", err)
	}
}
func TestApprovalNeededEventHasCorrectFields(t *testing.T) {
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}
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
	if ev.ToolID != "edit" {
		t.Errorf("expected tool_id edit, got %s", ev.ToolID)
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
	cfg := config.LoopConfig{MaxTurns: 2, DoomThreshold: 3, EditEnabled: true}
	engine := newTestEngine(t, cfg)

	client := &mockInferClient{tokens: toolCallTokens("edit", `{"path":"/tmp/test.txt"}`)}
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
				t.Errorf("approval scope = %q, want %q", ev.ApprovalScope, approvals.ApprovalScopeOnce)
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

func TestApplyApprovalClaimsResponseBeforeRememberingRule(t *testing.T) {
	engine := &Engine{
		evl: approvals.NewEvaluator(approvals.DefaultLayer()),
		pending: map[string]chan approvals.ApprovalResponse{
			"approval-1": make(chan approvals.ApprovalResponse, 1),
		},
		pendingRules: map[string]approvals.Rule{
			"approval-1": {ToolID: "edit", Decision: approvals.Allowed, Source: "test"},
		},
	}
	if err := engine.ApplyApproval("approval-1", approvals.ApprovalResponse{Decision: approvals.Allowed, Remember: true}); err != nil {
		t.Fatalf("first ApplyApproval: %v", err)
	}
	if err := engine.ApplyApproval("approval-1", approvals.ApprovalResponse{Decision: approvals.Allowed, Remember: true}); err == nil {
		t.Fatal("duplicate ApplyApproval unexpectedly succeeded")
	}
	decision, _ := engine.evl.Evaluate("edit", "")
	if decision != approvals.Allowed {
		t.Fatalf("session rule decision = %v, want allowed", decision)
	}
}
