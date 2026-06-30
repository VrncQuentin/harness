// Package agentloop provides the M4 native loop engine: send conversation
// to the model, parse tool calls, dispatch to the tool registry, inject
// results, and repeat until stop/limit/cancel.
package agentloop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	// Terminate is the reason the loop stopped, set on the final event.
	Terminate string `json:"terminate,omitempty"`
}

const (
	EvtDone       = "done"
	EvtError      = "error"
	EvtText       = "text"
	EvtToolCall   = "tool_call"
	EvtToolResult = "tool_result"
	EvtLimit      = "limit"
	EvtDoom       = "doom_loop"
	EvtCancel     = "cancelled"
)

// Engine orchestrates the agent loop.
type Engine struct {
	infer    inference.Client
	registry *tools.Registry
	loopCfg  config.LoopConfig
	toolCtx  tools.Context
}

// NewEngine creates a loop engine with the given dependencies.
func NewEngine(
	infer inference.Client,
	registry *tools.Registry,
	loopCfg config.LoopConfig,
	toolCtx tools.Context,
) *Engine {
	return &Engine{
		infer:    infer,
		registry: registry,
		loopCfg:  loopCfg,
		toolCtx:  toolCtx,
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
			e.emit(evch, Event{Turn: turns, Type: EvtCancel, Terminate: EvtCancel})
			return ErrCancelled
		}

		// Assemble tool schemas for this turn, respecting enable toggles.
		var reqTools []inference.Tool
		for _, t := range e.registry.List() {
			if !e.isToolEnabled(t.ID()) {
				continue
			}
			reqTools = append(reqTools, inference.Tool{
				Type: "function",
				Function: inference.ToolDefinition{
					Name:        t.ID(),
					Description: t.Description(),
					Parameters:  t.Schema(),
				},
			})
		}

		req := inference.CompletionRequest{
			Model:    "local",
			Messages: messages,
			Stream:   true,
			Tools:    reqTools,
		}

		tokenCh, err := e.infer.Complete(ctx, req)
		if err != nil {
			e.emit(evch, Event{Turn: turns, Type: EvtError, Content: err.Error(), Terminate: EvtError})
			return fmt.Errorf("agentloop: complete: %w", err)
		}

		// Accumulate text and tool call deltas.
		var assistantText strings.Builder
		var toolCallSlots []*accumulatedToolCall

		for tok := range tokenCh {
			if tok.Err != nil {
				e.emit(evch, Event{Turn: turns, Type: EvtError, Content: tok.Err.Error(), Terminate: EvtError})
				return fmt.Errorf("agentloop: token stream: %w", tok.Err)
			}
			if tok.Done {
				break
			}
			if tok.Content != "" {
				assistantText.WriteString(tok.Content)
				e.emit(evch, Event{Turn: turns, Type: EvtText, Content: tok.Content})
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

		// If no tool calls, the model responded with plain text.
		if len(toolCallSlots) == 0 {
			e.emit(evch, Event{Turn: turns, Type: EvtDone, Terminate: EvtDone})
			return nil
		}

		// Check turn limit.
		if turns >= e.loopCfg.MaxTurns {
			e.emit(evch, Event{Turn: turns, Type: EvtLimit, Terminate: EvtLimit,
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
			e.emit(evch, Event{Turn: turns, Type: EvtDoom, Terminate: EvtDoom,
				Content: "Repeated identical tool calls detected — stopping to avoid loop"})
			return ErrDoomLoop
		}

		// Dispatch tool calls and inject results.
		for _, tc := range assistantMsg.ToolCalls {
			if err := ctx.Err(); err != nil {
				e.emit(evch, Event{Turn: turns, Type: EvtCancel, Terminate: EvtCancel})
				return ErrCancelled
			}

			tool := e.registry.Get(tc.Function.Name)
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				args = map[string]any{}
			}

			e.emit(evch, Event{
				Turn:     turns,
				Type:     EvtToolCall,
				ToolID:   tc.Function.Name,
				ToolArgs: tc.Function.Arguments,
			})

			var res tools.Result
			if tool == nil || !e.isToolEnabled(tc.Function.Name) {
				res = tools.Result{Error: fmt.Sprintf("tool %q not available", tc.Function.Name)}
			} else {
				start := time.Now()
				res = tool.Execute(ctx, e.toolCtx, args)
				slog.Info("tool executed",
					"tool", tc.Function.Name,
					"duration_ms", time.Since(start).Milliseconds(),
					"has_error", res.Error != "",
				)
			}

			e.emit(evch, Event{
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

func (e *Engine) emit(evch chan<- Event, ev Event) {
	select {
	case evch <- ev:
	default:
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
	default:
		return true
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
