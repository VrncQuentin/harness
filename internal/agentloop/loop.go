// Package agentloop provides the M4 native loop engine: send conversation
// to the model, parse tool calls, dispatch to the tool registry, inject
// results, and repeat until stop/limit/cancel.
//
// M7 adds an optional approval layer: when an approvals.Evaluator is
// configured, destructive tool calls are checked before dispatch. Allowed
// calls proceed immediately; Denied calls inject a tool-error; Ask calls
// pause the loop, emit an approval event, and wait for the caller to
// apply a decision via ApplyApproval.
package agentloop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/vrnc/harness/internal/approvals"
	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/inference"
	"github.com/vrnc/harness/internal/tools"
)

// Event is a typed loop event emitted to the caller for UI display.
type Event struct {
	Turn    int    `json:"turn"`
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`

	ToolID     string `json:"tool_id,omitempty"`
	ToolArgs   string `json:"tool_args,omitempty"`
	ToolResult string `json:"tool_result,omitempty"`
	ToolError  string `json:"tool_error,omitempty"`

	// ApprovalID links an approval-needed event to its response.
	ApprovalID string `json:"approval_id,omitempty"`

	// Terminate is the reason the loop stopped, set on the final event.
	Terminate string `json:"terminate,omitempty"`
}

const (
	EvtDone           = "done"
	EvtError          = "error"
	EvtText           = "text"
	EvtToolCall       = "tool_call"
	EvtToolResult     = "tool_result"
	EvtLimit          = "limit"
	EvtDoom           = "doom_loop"
	EvtCancel         = "cancelled"
	EvtApprovalNeeded = "approval_needed"
	EvtApproval       = "approval"
)

const defaultApprovalTimeout = 5 * time.Minute

// ErrApprovalTimeout is returned when the caller does not apply an
// approval decision within the allowed window.
var ErrApprovalTimeout = errors.New("agentloop: approval timeout")

// MetricsRecorder is the narrow metric surface used by Engine.
type MetricsRecorder interface {
	LoopTurn() error
	ToolCall(string) error
	ToolCallError(string) error
	ToolCallErrorRate(string, bool) error
}

// Engine orchestrates the agent loop.
type Engine struct {
	infer       inference.Client
	registry    *tools.Registry
	toolSchemas []registeredToolSchema
	loopCfg     config.LoopConfig
	toolCtx     tools.Context
	metrics     MetricsRecorder

	approvalTimeout time.Duration

	// evl is the optional M7 permission evaluator. When nil, no
	// approval checks are performed (all tools dispatch immediately).
	evl *approvals.Evaluator

	// pending guards the approval routing map and counter.
	pendingMu sync.Mutex
	// pending maps approval IDs to response channels.
	pending map[string]chan approvals.ApprovalResponse
	// pendingRules maps approval IDs to the session rule that should
	// be added when the user chooses "always".
	pendingRules map[string]approvals.Rule
	// approvalSeq is a monotonic counter for generating unique IDs.
	approvalSeq int
}

// NewEngine creates a loop engine with the given dependencies.
func NewEngine(
	infer inference.Client,
	registry *tools.Registry,
	loopCfg config.LoopConfig,
	toolCtx tools.Context,
) *Engine {
	return &Engine{
		infer:           infer,
		registry:        registry,
		toolSchemas:     buildToolSchemas(registry),
		loopCfg:         loopCfg,
		toolCtx:         toolCtx,
		approvalTimeout: defaultApprovalTimeout,
	}
}

type registeredToolSchema struct {
	id   string
	tool inference.Tool
}

func buildToolSchemas(registry *tools.Registry) []registeredToolSchema {
	if registry == nil {
		return nil
	}
	registered := registry.List()
	schemas := make([]registeredToolSchema, 0, len(registered))
	for _, t := range registered {
		schemas = append(schemas, registeredToolSchema{
			id: t.ID(),
			tool: inference.Tool{
				Type: "function",
				Function: inference.ToolDefinition{
					Name:        t.ID(),
					Description: t.Description(),
					Parameters:  t.Schema(),
				},
			},
		})
	}
	return schemas
}

// WithApprovals installs an M7 permission evaluator. When nil (the
// default), no approval checks are performed. Call before Run().
func (e *Engine) WithApprovals(evl *approvals.Evaluator) *Engine {
	e.evl = evl
	return e
}

// WithMetrics installs optional loop/tool metrics recording. Call before Run().
func (e *Engine) WithMetrics(rec MetricsRecorder) *Engine {
	e.metrics = rec
	return e
}

// WithApprovalTimeout sets how long the loop waits for approval event delivery
// and a user decision. Non-positive values restore the default.
func (e *Engine) WithApprovalTimeout(d time.Duration) *Engine {
	if d <= 0 {
		d = defaultApprovalTimeout
	}
	e.approvalTimeout = d
	return e
}

// ApplyApproval delivers the user's decision for the approval event
// identified by approvalID. Returns an error when the id is unknown
// (already answered, timed out, or never emitted).
func (e *Engine) ApplyApproval(approvalID string, resp approvals.ApprovalResponse) error {
	e.pendingMu.Lock()
	ch, ok := e.pending[approvalID]
	rule, hasRule := e.pendingRules[approvalID]
	e.pendingMu.Unlock()
	if !ok {
		return fmt.Errorf("agentloop: unknown approval id %q", approvalID)
	}
	// Only add a session rule when the user explicitly chose "always".
	if hasRule && resp.Remember {
		rule.Decision = resp.Decision
		e.evl.AddSessionRule(rule)
	}
	select {
	case ch <- resp:
		return nil
	default:
		return fmt.Errorf("agentloop: approval id %q already resolved", approvalID)
	}
}

// ErrLoopLimitReached is returned when the loop hits MaxTurns.
var ErrLoopLimitReached = errors.New("agentloop: max turns reached")

// ErrDoomLoop is returned when the loop detects repeated identical tool calls.
var ErrDoomLoop = errors.New("agentloop: repeated identical tool calls detected")

// ErrCancelled is returned when the context is cancelled.
var ErrCancelled = errors.New("agentloop: cancelled")

// Run executes the agent loop: send conversation, parse tool calls,
// dispatch to registry, inject results, repeat. Events are sent on evch
// for UI consumption; the channel is closed when the loop finishes.
func (e *Engine) Run(ctx context.Context, messages []inference.Message, evch chan<- Event) error {
	defer close(evch)
	turns := 0
	var lastFPs [][]byte // per-turn fingerprints for doom detection

	for {
		if err := ctx.Err(); err != nil {
			e.emit(ctx, evch, Event{Turn: turns, Type: EvtCancel, Terminate: EvtCancel})
			return ErrCancelled
		}

		// Select cached tool schemas for this request, respecting enable toggles.
		var reqTools []inference.Tool
		for _, schema := range e.toolSchemas {
			if !e.isToolEnabled(schema.id) {
				continue
			}
			reqTools = append(reqTools, schema.tool)
		}

		req := inference.CompletionRequest{
			Model:    "local",
			Messages: messages,
			Stream:   true,
			Tools:    reqTools,
		}

		tokenCh, err := e.infer.Complete(ctx, req)
		if err != nil {
			e.emit(ctx, evch, Event{Turn: turns, Type: EvtError, Content: err.Error(), Terminate: EvtError})
			return fmt.Errorf("agentloop: complete: %w", err)
		}

		// Accumulate text and tool call deltas.
		var assistantText strings.Builder
		var toolCallSlots []*accumulatedToolCall

		for tok := range tokenCh {
			if tok.Err != nil {
				e.emit(ctx, evch, Event{Turn: turns, Type: EvtError, Content: tok.Err.Error(), Terminate: EvtError})
				return fmt.Errorf("agentloop: token stream: %w", tok.Err)
			}
			if tok.Done {
				break
			}
			if tok.Content != "" {
				assistantText.WriteString(tok.Content)
				e.emit(ctx, evch, Event{Turn: turns, Type: EvtText, Content: tok.Content})
			}
			if tok.ToolCallDelta != nil {
				slot := resolveSlot(&toolCallSlots, tok.ToolCallDelta.Index)
				if tok.ToolCallDelta.ID != "" {
					slot.ID = tok.ToolCallDelta.ID
				}
				if tok.ToolCallDelta.Name != "" {
					slot.Name = tok.ToolCallDelta.Name
				}
				slot.Arguments.WriteString(tok.ToolCallDelta.Arguments)
			}
		}

		turns++
		e.recordLoopTurn()

		// If no tool calls, the model responded with plain text.
		if len(toolCallSlots) == 0 {
			e.emit(ctx, evch, Event{Turn: turns, Type: EvtDone, Terminate: EvtDone})
			return nil
		}

		// Check turn limit.
		if turns >= e.loopCfg.MaxTurns {
			e.emit(ctx, evch, Event{Turn: turns, Type: EvtLimit, Terminate: EvtLimit,
				Content: fmt.Sprintf("Loop limit reached after %d turns", turns)})
			return ErrLoopLimitReached
		}

		// Build assistant message with tool calls.
		assistantMsg := inference.Message{Role: "assistant"}
		if assistantText.Len() > 0 {
			assistantMsg.Content = assistantText.String()
		}
		for _, slot := range toolCallSlots {
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, inference.ToolCall{
				ID:   slot.ID,
				Type: "function",
				Function: inference.ToolCallFunction{
					Name:      slot.Name,
					Arguments: slot.Arguments.String(),
				},
			})
		}
		messages = append(messages, assistantMsg)

		// Doom-loop detection: fingerprint this turn's tool calls and
		// check if the same fingerprint appears in the last N turns.
		turnFP := turnFingerprint(assistantMsg.ToolCalls)
		lastFPs = append(lastFPs, turnFP)
		if len(lastFPs) > e.loopCfg.DoomThreshold {
			lastFPs = lastFPs[1:]
		}
		if len(lastFPs) >= e.loopCfg.DoomThreshold && allEqualFP(lastFPs) {
			e.emit(ctx, evch, Event{Turn: turns, Type: EvtDoom, Terminate: EvtDoom,
				Content: "Repeated identical tool calls detected — stopping to avoid loop"})
			return ErrDoomLoop
		}

		// Dispatch tool calls and inject results.
		for _, tc := range assistantMsg.ToolCalls {
			if err := ctx.Err(); err != nil {
				e.emit(ctx, evch, Event{Turn: turns, Type: EvtCancel, Terminate: EvtCancel})
				return ErrCancelled
			}

			tool := e.registry.Get(tc.Function.Name)
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				args = map[string]any{}
			}

			e.emit(ctx, evch, Event{
				Turn:     turns,
				Type:     EvtToolCall,
				ToolID:   tc.Function.Name,
				ToolArgs: tc.Function.Arguments,
			})

			var res tools.Result
			if tool == nil || !e.isToolEnabled(tc.Function.Name) {
				res = tools.Result{Error: fmt.Sprintf("tool %q not available", tc.Function.Name)}
			} else {
				// M7: check approvals before dispatch.
				decision, err := e.checkApproval(ctx, evch, turns, tc.Function.Name, args)
				if err != nil {
					e.emit(ctx, evch, Event{Turn: turns, Type: EvtError, Content: err.Error(), Terminate: EvtError})
					return err
				}
				switch decision {
				case approvals.Denied:
					res = tools.Result{Error: fmt.Sprintf("tool %q denied by permission policy", tc.Function.Name)}
					e.emit(ctx, evch, Event{
						Turn:      turns,
						Type:      EvtApproval,
						ToolID:    tc.Function.Name,
						ToolError: "denied",
					})
				case approvals.Allowed, approvals.Ask:
					start := time.Now()
					res = tool.Execute(ctx, e.toolCtx, args)
					slog.Info("tool executed",
						"tool", tc.Function.Name,
						"duration_ms", time.Since(start).Milliseconds(),
						"has_error", res.Error != "",
					)
				}
			}

			e.recordToolCall(tc.Function.Name, res.Error != "")

			e.emit(ctx, evch, Event{
				Turn:       turns,
				Type:       EvtToolResult,
				ToolID:     tc.Function.Name,
				ToolResult: res.Content,
				ToolError:  res.Error,
			})

			// Inject tool result into conversation.
			resultContent := res.Content
			if res.Error != "" {
				resultContent = "ERROR: " + res.Error
			}
			messages = append(messages, inference.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    resultContent,
			})
		}
	}
}

// checkApproval evaluates the permission policy and, when the decision
// is Ask, emits an approval-needed event and waits for the caller to
// apply a decision via ApplyApproval.
func (e *Engine) checkApproval(ctx context.Context, evch chan<- Event, turn int, toolID string, args map[string]any) (approvals.Decision, error) {
	if e.evl == nil {
		return approvals.Allowed, nil
	}

	cmdArg := ""
	if toolID == "shell_exec" {
		if s, ok := args["command"].(string); ok {
			cmdArg = s
		}
	}

	dec, src := e.evl.Evaluate(toolID, cmdArg)
	if dec != approvals.Ask {
		slog.Debug("approval check", "tool", toolID, "decision", dec, "source", src)
		return dec, nil
	}

	// Generate a unique approval ID and set up the response channel.
	e.pendingMu.Lock()
	if e.pending == nil {
		e.pending = make(map[string]chan approvals.ApprovalResponse)
	}
	if e.pendingRules == nil {
		e.pendingRules = make(map[string]approvals.Rule)
	}
	e.approvalSeq++
	approvalID := fmt.Sprintf("%s-%d-%d", toolID, turn, e.approvalSeq)
	ch := make(chan approvals.ApprovalResponse, 1)
	e.pending[approvalID] = ch

	// Store a pending session rule for the "always" decision.
	// For shell_exec, store the exact command string so "always"
	// only matches that specific command, not a broad prefix.
	cmdPattern := ""
	if toolID == "shell_exec" && cmdArg != "" {
		cmdPattern = cmdArg
	}
	e.pendingRules[approvalID] = approvals.Rule{
		ToolID:         toolID,
		CommandPattern: cmdPattern,
		Decision:       approvals.Allowed,
		Source:         "session: always allowed",
	}
	e.pendingMu.Unlock()

	defer func() {
		e.pendingMu.Lock()
		delete(e.pending, approvalID)
		delete(e.pendingRules, approvalID)
		e.pendingMu.Unlock()
	}()

	timeout := e.approvalTimeout
	if timeout <= 0 {
		timeout = defaultApprovalTimeout
	}

	// Emit approval-needed event. This event is a state transition: if it is
	// dropped, the caller cannot render an approval card and the loop cannot
	// make progress. Wait for delivery, but still bound the wait.
	if err := emitApprovalNeeded(ctx, evch, Event{
		Turn:       turn,
		Type:       EvtApprovalNeeded,
		ToolID:     toolID,
		ToolArgs:   jsonString(args),
		ApprovalID: approvalID,
		Content:    fmt.Sprintf("%s requires approval (%s)", toolID, src),
	}, timeout); err != nil {
		return approvals.Denied, err
	}

	// Wait for user decision with timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return approvals.Denied, ErrCancelled
	case <-timer.C:
		return approvals.Denied, ErrApprovalTimeout
	case resp := <-ch:
		slog.Debug("approval resolved", "tool", toolID, "id", approvalID, "decision", resp.Decision, "remember", resp.Remember)
		if resp.Decision == approvals.Denied {
			e.emit(ctx, evch, Event{
				Turn:       turn,
				Type:       EvtApproval,
				ToolID:     toolID,
				ToolError:  "denied",
				ApprovalID: approvalID,
			})
		} else {
			e.emit(ctx, evch, Event{
				Turn:       turn,
				Type:       EvtApproval,
				ToolID:     toolID,
				ApprovalID: approvalID,
			})
		}
		return resp.Decision, nil
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func emitApprovalNeeded(ctx context.Context, evch chan<- Event, ev Event, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case evch <- ev:
		return nil
	case <-ctx.Done():
		return ErrCancelled
	case <-timer.C:
		return ErrApprovalTimeout
	}
}
func (e *Engine) emit(ctx context.Context, evch chan<- Event, ev Event) {
	if ev.Type == EvtText {
		select {
		case evch <- ev:
		default:
		}
		return
	}
	select {
	case evch <- ev:
	case <-ctx.Done():
	}
}

func (e *Engine) isToolEnabled(id string) bool {
	switch id {
	case "file_read":
		return e.loopCfg.FileReadEnabled
	case "file_list":
		return e.loopCfg.FileListEnabled
	case "file_write":
		return e.loopCfg.FileWriteEnabled
	case "shell_exec":
		return e.loopCfg.ShellExecEnabled
	case "web_search":
		return e.loopCfg.WebSearchEnabled
	default:
		return false
	}
}

type accumulatedToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func resolveSlot(slots *[]*accumulatedToolCall, index int) *accumulatedToolCall {
	for len(*slots) <= index {
		*slots = append(*slots, &accumulatedToolCall{})
	}
	return (*slots)[index]
}

// turnFingerprint produces a hash of the tool calls in a single turn so
// the doom-loop detector can compare turns (not individual calls).
func turnFingerprint(calls []inference.ToolCall) []byte {
	h := sha256.New()
	for _, tc := range calls {
		h.Write([]byte(tc.Function.Name))
		h.Write([]byte{0})
		h.Write([]byte(tc.Function.Arguments))
		h.Write([]byte{0})
	}
	return h.Sum(nil)
}

func allEqualFP(items [][]byte) bool {
	if len(items) <= 1 {
		return false
	}
	first := items[0]
	for _, item := range items[1:] {
		if string(item) != string(first) {
			return false
		}
	}
	return true
}

func (e *Engine) recordLoopTurn() {
	if e.metrics != nil {
		_ = e.metrics.LoopTurn()
	}
}

func (e *Engine) recordToolCall(tool string, failed bool) {
	if e.metrics == nil {
		return
	}
	_ = e.metrics.ToolCall(tool)
	_ = e.metrics.ToolCallErrorRate(tool, failed)
	if failed {
		_ = e.metrics.ToolCallError(tool)
	}
}
