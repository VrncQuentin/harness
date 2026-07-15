package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
)

// ChatMessage is a single message in the server-owned chat transcript.
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
//
// M3 extends Run to take and return a session id so the browser can pin
// every turn to one persistent session and so the assistant turn is
// captured by the session manager as it streams.
type ChatRunner interface {
	Run(ctx context.Context, agent, sessionID string, conversation []ChatMessage) (string, <-chan ChatToken, error)
}

// Sentinel errors a ChatRunner may return so the handler can pick the
// right HTTP status without importing the inference or queue packages.
var (
	ErrChatNoAgent     = errors.New("no active agent: pick one on /agents or send {\"agent\": ...} in the request body")
	ErrChatQueueFull   = errors.New("inference queue is at capacity, try again in a moment")
	ErrChatUnavailable = errors.New("chat backend is not available")
)

// Sentinel errors a SessionStore may return.
var (
	ErrSessionUnavailable      = errors.New("session manager not available")
	ErrSessionUnknown          = errors.New("session unknown")
	ErrSessionConversationLost = errors.New("session conversation history not available - only the summary survives in git")
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
	ActiveAgent    string
	RecentSessions []chatResumeRow
	ResumeErr      string
	StreamID       string
}

type chatResumeRow struct {
	ID      string
	Agent   string
	SavedAt string
	SaveSeq int
}

// RecentSessionLimit is the cap on how many records the resume picker shows.
const RecentSessionLimit = 10

// handleChat renders the /chat page (GET only).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := chatView{basePage: s.newBasePage("chat"), StreamID: newEventStreamID()}
	data.Configured = s.getChatRunner() != nil
	if reg := s.agentRegistry(); reg != nil {
		data.ActiveAgent = reg.Active()
		if list, err := reg.List(); err == nil {
			data.HasAgents = len(list) > 0
		}
	}
	if data.Configured && data.ActiveAgent != "" {
		data.RecentSessions, data.ResumeErr = s.chatResumeRows(data.ActiveAgent)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.chatTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) chatResumeRows(agent string) ([]chatResumeRow, string) {
	store := s.getSessionStore()
	if store == nil {
		return nil, "session manager not available"
	}
	records, err := store.Records(agent)
	if err != nil {
		return nil, err.Error()
	}
	if len(records) > RecentSessionLimit {
		records = records[:RecentSessionLimit]
	}
	rows := make([]chatResumeRow, 0, len(records))
	for _, rec := range records {
		rows = append(rows, chatResumeRow{
			ID:      rec.ID,
			Agent:   rec.Agent,
			SavedAt: rec.SavedAt.Format("2006-01-02 15:04"),
			SaveSeq: rec.SaveSeq,
		})
	}
	return rows, ""
}

// chatStreamRequest is the JSON body of POST /chat/stream. Agent is
// optional; an empty value defers to the runner's active-agent default.
// SessionID is also optional - empty means "start a new session"; a
// non-empty value resumes the session minted on a previous turn.
type chatStreamRequest struct {
	Agent     string        `json:"agent,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	Messages  []ChatMessage `json:"messages"`
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

	sessionID, tokens, err := runner.Run(ctx, req.Agent, req.SessionID, req.Messages)
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

	// Emit the session id as the first SSE event so the browser can
	// pin subsequent stream and save calls to the same id. This always
	// lands before any token frames, giving the client a chance to
	// stash the id even if the user navigates away mid-stream.
	if sessionID != "" {
		writeChatSSEEvent(w, flusher, "session", map[string]any{"id": sessionID})
	}

	for tok := range tokens {
		if tok.Err != nil {
			writeChatSSEError(w, flusher, tok.Err.Error())
			return
		}
		if tok.Done {
			writeChatSSEDone(w, flusher)
			return
		}
		if tok.Content == "" {
			continue
		}
		if !writeChatSSEContent(w, flusher, tok.Content) {
			return
		}
	}
	// Channel closed without an explicit Done token. Emit one so the
	// client always sees a terminal frame and can re-enable input.
	writeChatSSEDone(w, flusher)
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

func chatTextSSEData(text string) string {
	return sseData(html.EscapeString(text))
}

// writeChatSSEContent emits a single SSE data frame with plain-text
// content. No JSON wrapping — the browser inserts the text directly.
func writeChatSSEContent(w http.ResponseWriter, flusher http.Flusher, content string) bool {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", chatTextSSEData(content)); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// writeChatSSEDone emits a named event signalling the stream is complete.
func writeChatSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = fmt.Fprint(w, "event: chat-done\ndata: \n\n")
	flusher.Flush()
}

// writeChatSSEError emits a named event carrying an error message.
func writeChatSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) bool {
	if _, err := fmt.Fprintf(w, "event: chat-error\ndata: %s\n\n", chatTextSSEData(msg)); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// writeChatSSEEvent is writeChatSSE with an explicit event: name. Used
// to tag the session-id frame; JSON is kept here since the session id
// is a small structured payload.
func writeChatSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload map[string]any) bool {
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// chatSendView is the template data for the chat-send-fragment partial.
// The server renders the user message and an empty assistant placeholder,
// then streams tokens to that placeholder through /chat/events.
type chatSendView struct {
	UserContent string
	SessionID   string
	Agent       string
	StreamID    string
}

// chatSendMaxBytes caps the form body. A single message plus small
// hidden fields fits well within 32 KiB.
const chatSendMaxBytes = 32 * 1024

// handleChatSend is the htmx POST handler for the chat input form. It
// renders the user's message and an empty assistant placeholder into
// the transcript via htmx swap, then starts server-owned token streaming.
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.getChatRunner() == nil {
		http.Error(w, "chat backend not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, chatSendMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(r.FormValue("message"))
	if msg == "" {
		http.Error(w, "message must not be empty", http.StatusBadRequest)
		return
	}
	agent := strings.TrimSpace(r.FormValue("agent"))
	sessionID := strings.TrimSpace(r.FormValue("session_id"))
	streamID := strings.TrimSpace(r.FormValue("stream_id"))

	var conversation []ChatMessage
	if sessionID != "" {
		if store := s.getSessionStore(); store != nil {
			if live, err := store.LiveConversation(sessionID); err == nil {
				conversation = live
			}
		}
	}
	conversation = append(conversation, ChatMessage{Role: "user", Content: msg})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.chatTmpl.ExecuteTemplate(w, "chat-send-fragment", chatSendView{
		UserContent: msg,
		SessionID:   sessionID,
		Agent:       agent,
		StreamID:    streamID,
	}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	// Start the runner in the background and broadcast tokens via SSE.
	runner := s.getChatRunner()
	if runner != nil {
		ctx, cancel := context.WithCancel(s.asyncContext())
		go func() {
			defer cancel()
			s.streamChatTokens(ctx, runner, agent, sessionID, streamID, conversation)
		}()
	}
}

// streamChatTokens runs the chat runner in a goroutine and broadcasts
// tokens, done, and error events to the chat SSE subscribers.
func (s *Server) streamChatTokens(ctx context.Context, runner ChatRunner, agent, sessionID, streamID string, conversation []ChatMessage) {
	newID, tokens, err := runner.Run(ctx, agent, sessionID, conversation)
	if err != nil {
		s.broadcastChatSSE(streamID, fmt.Sprintf("event: chat-error\ndata: %s\n\n", sseData(err.Error())))
		return
	}
	if newID != "" && newID != sessionID {
		s.broadcastChatSSE(streamID, fmt.Sprintf("event: chat-session\ndata: %s\n\n", sseData(chatSessionOOB(newID))))
	}
	for tok := range tokens {
		if tok.Err != nil {
			s.broadcastChatSSE(streamID, fmt.Sprintf("event: chat-error\ndata: %s\n\n", sseData(tok.Err.Error())))
			return
		}
		if tok.Done {
			s.broadcastChatSSE(streamID, "event: chat-done\ndata: \n\n")
			return
		}
		if tok.Content != "" {
			s.broadcastChatSSE(streamID, fmt.Sprintf("event: chat-token\ndata: %s\n\n", chatTextSSEData(tok.Content)))
		}
	}
	s.broadcastChatSSE(streamID, "event: chat-done\ndata: \n\n")
}

func chatSessionOOB(id string) string {
	return fmt.Sprintf(`<code id="chat-session-id" hx-swap-oob="true">%s</code>
<input type="hidden" id="chat-session-input" name="session_id" hx-swap-oob="true" value="%s">`, id, id)
}

// handleChatEvents is the SSE endpoint for chat token streaming. The
// chat page connects via hx-ext="sse" sse-connect="/chat/events" and
// the assistant placeholder consumes tokens via sse-swap.
func (s *Server) handleChatEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if _, err := fmt.Fprint(w, ": chat-connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	streamID := strings.TrimSpace(r.URL.Query().Get("stream_id"))
	ch := make(chan string, 8)
	s.chatSSEClients.Store(ch, streamID)
	defer s.chatSSEClients.Delete(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// broadcastChatSSE sends an SSE frame to matching chat SSE subscribers. The
// frame must be a complete SSE frame (event + data lines with trailing
// blank line). Non-blocking; a slow client drops this frame.
func (s *Server) broadcastChatSSE(streamID, frame string) {
	s.chatSSEClients.Range(func(key, value any) bool {
		ch, ok := key.(chan string)
		if !ok {
			return true
		}
		clientStreamID, _ := value.(string)
		if streamID != "" && clientStreamID != streamID {
			return true
		}
		select {
		case ch <- frame:
		default:
		}
		return true
	})
}
