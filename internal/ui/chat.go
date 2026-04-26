package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ChatMessage is a single message in the browser-side chat transcript.
// Mirrors the OpenAI shape so the JSON exchanged with the page can be
// re-fed into the API server unchanged.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatToken is one streamed delta from the model. Done marks the final
// frame; Err carries a fatal stream error and supersedes Content.
type ChatToken struct {
	Content string
	Done    bool
	Err     error
}

// ChatRunner drives a chat completion through the assembler + queue +
// inference pipeline. Concrete implementations live in internal/runtime;
// the UI deliberately avoids importing inference/queue, so the runner
// translates between those layers and the small DTOs above. Same pattern
// as AgentRegistry.
type ChatRunner interface {
	Run(ctx context.Context, agent string, conversation []ChatMessage) (<-chan ChatToken, error)
}

// Sentinel errors a ChatRunner may return so the handler can pick the
// right HTTP status without importing the inference or queue packages.
var (
	ErrChatNoAgent     = errors.New("no active agent: pick one on /agents or send {\"agent\": ...} in the request body")
	ErrChatQueueFull   = errors.New("inference queue is at capacity, try again in a moment")
	ErrChatUnavailable = errors.New("chat backend is not available")
)

// SetChatRunner installs the runner used by /chat/stream. Safe to leave
// unset; the page then renders a "not configured" state instead of a
// chat input.
func (s *Server) SetChatRunner(r ChatRunner) {
	s.chatRunnerMu.Lock()
	s.chatRunner = r
	s.chatRunnerMu.Unlock()
}

func (s *Server) getChatRunner() ChatRunner {
	s.chatRunnerMu.RLock()
	defer s.chatRunnerMu.RUnlock()
	return s.chatRunner
}

// chatView is the template context for the /chat page.
type chatView struct {
	basePage
	// Configured is true when a ChatRunner has been wired in. False
	// flips the page to a setup CTA pointing at /config.
	Configured bool
	// HasAgents is true when the registry has at least one agent. False
	// flips the page to a CTA pointing at /agents.
	HasAgents bool
	// ActiveAgent is the current default agent (empty string means none).
	ActiveAgent string
}

// handleChat renders the /chat page (GET only). The transcript itself
// lives client-side in JS - the server only renders the shell and the
// streaming endpoint.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := chatView{basePage: s.newBasePage("chat")}
	data.Configured = s.getChatRunner() != nil
	if reg := s.agentRegistry(); reg != nil {
		data.ActiveAgent = reg.Active()
		if list, err := reg.List(); err == nil {
			data.HasAgents = len(list) > 0
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.chatTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// chatStreamRequest is the JSON body of POST /chat/stream. Agent is
// optional; an empty value defers to the runner's active-agent default.
type chatStreamRequest struct {
	Agent    string        `json:"agent,omitempty"`
	Messages []ChatMessage `json:"messages"`
}

// chatStreamMaxBytes caps the request body so a runaway transcript does
// not exhaust memory before we even decode it. 256 KiB covers thousands
// of conversation turns at typical sizes.
const chatStreamMaxBytes = 256 * 1024

// handleChatStream consumes a POSTed conversation, dispatches it to the
// chat runner, and streams tokens back as `text/event-stream` JSON
// chunks. The browser reads the stream with fetch + a body reader since
// EventSource is GET only and we need a JSON body.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runner := s.getChatRunner()
	if runner == nil {
		writeChatJSONError(w, http.StatusServiceUnavailable, "chat backend not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, chatStreamMaxBytes)
	var req chatStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeChatJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeChatJSONError(w, http.StatusBadRequest, "messages must not be empty")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeChatJSONError(w, http.StatusInternalServerError, "streaming unsupported by this transport")
		return
	}

	// Wrap in a cancellable child so an early return propagates to the
	// runner (and through it to the queue + inference client). Without
	// this, a write failure would leave the producer running until the
	// model completes on its own.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	tokens, err := runner.Run(ctx, req.Agent, req.Messages)
	if err != nil {
		writeChatJSONError(w, statusFromChatErr(err), err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for tok := range tokens {
		if tok.Err != nil {
			writeChatSSE(w, flusher, map[string]any{"error": tok.Err.Error()})
			return
		}
		if tok.Done {
			writeChatSSE(w, flusher, map[string]any{"done": true})
			return
		}
		if tok.Content == "" {
			continue
		}
		if !writeChatSSE(w, flusher, map[string]any{"content": tok.Content}) {
			return
		}
	}
	// Channel closed without an explicit Done token. Emit one so the
	// client always sees a terminal frame and can re-enable input.
	writeChatSSE(w, flusher, map[string]any{"done": true})
}

func statusFromChatErr(err error) int {
	switch {
	case errors.Is(err, ErrChatNoAgent):
		return http.StatusBadRequest
	case errors.Is(err, ErrChatQueueFull):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrChatUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeChatJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeChatSSE encodes payload as a JSON object and emits a single SSE
// data event. Returns false on the first write error so callers can
// abort the stream.
func writeChatSSE(w http.ResponseWriter, flusher http.Flusher, payload map[string]any) bool {
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
