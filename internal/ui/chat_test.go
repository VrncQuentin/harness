package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// stubChatRunner is a test double that records the last Run invocation
// and either replays a scripted token sequence or returns a scripted
// pre-dispatch error.
type stubChatRunner struct {
	mu        sync.Mutex
	calls     int
	lastCtx   context.Context
	lastName  string
	lastID    string
	lastMsgs  []ChatMessage
	returnsID string

	preErr error
	tokens []ChatToken
}

func (r *stubChatRunner) Run(ctx context.Context, agent, sessionID string, conv []ChatMessage) (string, <-chan ChatToken, error) {
	r.mu.Lock()
	r.calls++
	r.lastCtx = ctx
	r.lastName = agent
	r.lastID = sessionID
	r.lastMsgs = append([]ChatMessage(nil), conv...)
	preErr := r.preErr
	tokens := append([]ChatToken(nil), r.tokens...)
	mintedID := r.returnsID
	if mintedID == "" {
		mintedID = sessionID
	}
	r.mu.Unlock()

	if preErr != nil {
		return "", nil, preErr
	}
	ch := make(chan ChatToken, len(tokens))
	for _, t := range tokens {
		ch <- t
	}
	close(ch)
	return mintedID, ch, nil
}

func (r *stubChatRunner) lastConversation() []ChatMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ChatMessage(nil), r.lastMsgs...)
}

// TestHandleChat_GETUnconfiguredShowsBackendCTA: with no runner wired we
// land in the setup CTA branch rather than rendering the chat shell.
func TestHandleChat_GETUnconfiguredShowsBackendCTA(t *testing.T) {
	s := NewServer(3000)

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Chat backend not ready") {
		t.Errorf("expected backend CTA, got:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `id="chat-form"`) {
		t.Errorf("did not expect chat form when unconfigured")
	}
}

// TestHandleChat_GETNoAgentsShowsAgentsCTA: runner wired, but the
// registry is empty - the page should send the user to /agents.
func TestHandleChat_GETNoAgentsShowsAgentsCTA(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})
	s.SetAgentRegistry(newStubRegistry(""))

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No agents yet") {
		t.Errorf("expected no-agents CTA, got:\n%s", rec.Body.String())
	}
}

// TestHandleChat_GETNoActiveAgentShowsActiveCTA: agents exist but the
// active selection is empty.
func TestHandleChat_GETNoActiveAgentShowsActiveCTA(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})
	s.SetAgentRegistry(newStubRegistry("",
		AgentInfo{Name: "coder"},
	))

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No active agent") {
		t.Errorf("expected no-active-agent CTA, got:\n%s", rec.Body.String())
	}
}

// TestHandleChat_GETHappyPathRendersForm: runner wired and an active
// agent set - we expect the chat shell with the agent name baked in.
func TestHandleChat_GETHappyPathRendersForm(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})
	s.SetAgentRegistry(newStubRegistry("coder",
		AgentInfo{Name: "coder"},
	))

	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="chat-form"`, `data-agent="coder"`, `Active agent`} {
		if !strings.Contains(body, want) {
			t.Errorf("chat body missing %q", want)
		}
	}
}

// TestHandleChat_RejectsNonGET ensures POST/PUT/etc. on /chat return 405
// instead of accidentally re-rendering or mutating anything.
func TestHandleChat_RejectsNonGET(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	s.handleChat(rec, httptest.NewRequest(http.MethodPost, "/chat", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// TestHandleChatStream_RejectsNonPOST: a stray GET should not be able
// to invoke the inference pipeline.
func TestHandleChatStream_RejectsNonPOST(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	s.handleChatStream(rec, httptest.NewRequest(http.MethodGet, "/chat/stream", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// TestHandleChatStream_NoRunnerReturns503 covers the case where the
// chat backend has not been wired in (e.g. memory repo invalid).
func TestHandleChatStream_NoRunnerReturns503(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleChatStream_BadJSONReturns400 covers a malformed body.
func TestHandleChatStream_BadJSONReturns400(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// TestHandleChatStream_EmptyMessagesReturns400: the runner is never
// invoked when the conversation is empty.
func TestHandleChatStream_EmptyMessagesReturns400(t *testing.T) {
	s := NewServer(3000)
	runner := &stubChatRunner{}
	s.SetChatRunner(runner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
	runner.mu.Lock()
	calls := runner.calls
	runner.mu.Unlock()
	if calls != 0 {
		t.Errorf("expected runner to be skipped on empty messages, got %d call(s)", calls)
	}
}

// TestHandleChatStream_SentinelStatusMapping verifies each ui sentinel
// error from the runner becomes the right HTTP status.
func TestHandleChatStream_SentinelStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no-agent", ErrChatNoAgent, http.StatusBadRequest},
		{"queue-full", ErrChatQueueFull, http.StatusTooManyRequests},
		{"unavailable", ErrChatUnavailable, http.StatusServiceUnavailable},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewServer(3000)
			s.SetChatRunner(&stubChatRunner{preErr: c.err})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			s.handleChatStream(rec, req)
			if rec.Code != c.want {
				t.Errorf("status: want %d, got %d (body %s)", c.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleChatStream_StreamsTokens drives a happy path: tokens go
// out as SSE frames, the conversation is forwarded verbatim, and the
// response carries a terminal done frame.
func TestHandleChatStream_StreamsTokens(t *testing.T) {
	s := NewServer(3000)
	runner := &stubChatRunner{
		tokens: []ChatToken{
			{Content: "hel"},
			{Content: "lo"},
			{Done: true},
		},
	}
	s.SetChatRunner(runner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(
		`{"agent":"coder","messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("expected event-stream content-type, got %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data: hel`,
		`data: lo`,
		`event: chat-done`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing frame %q, full body:\n%s", want, body)
		}
	}

	conv := runner.lastConversation()
	if len(conv) != 1 || conv[0].Role != "user" || conv[0].Content != "hi" {
		t.Errorf("forwarded conversation looks wrong: %#v", conv)
	}
	runner.mu.Lock()
	gotAgent := runner.lastName
	runner.mu.Unlock()
	if gotAgent != "coder" {
		t.Errorf("expected agent=coder, got %q", gotAgent)
	}
}

// TestHandleChatStream_TokenErrorEmitsErrorFrame: a mid-stream error
// from the runner should land as an SSE error frame, not crash.
func TestHandleChatStream_TokenErrorEmitsErrorFrame(t *testing.T) {
	s := NewServer(3000)
	runner := &stubChatRunner{
		tokens: []ChatToken{
			{Content: "partial"},
			{Err: errors.New("model exploded")},
		},
	}
	s.SetChatRunner(runner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(
		`{"messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: partial`) {
		t.Errorf("expected partial token in stream, got %s", body)
	}
	if !strings.Contains(body, `event: chat-error`) {
		t.Errorf("expected error frame in stream, got %s", body)
	}
	if !strings.Contains(body, `model exploded`) {
		t.Errorf("expected error message in stream, got %s", body)
	}
}

// TestHandleChatStream_SyntheticDoneOnClosedChannel: a runner that
// closes its channel without an explicit Done token should still
// produce a terminal frame so the browser re-enables input.
func TestHandleChatStream_SyntheticDoneOnClosedChannel(t *testing.T) {
	s := NewServer(3000)
	runner := &stubChatRunner{
		tokens: []ChatToken{
			{Content: "ok"},
		},
	}
	s.SetChatRunner(runner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/stream", strings.NewReader(
		`{"messages":[{"role":"user","content":"hi"}]}`,
	))
	req.Header.Set("Content-Type", "application/json")
	s.handleChatStream(rec, req)

	if !strings.Contains(rec.Body.String(), `event: chat-done`) {
		t.Errorf("expected synthetic done frame, got %s", rec.Body.String())
	}
}

// --- /chat/send tests --------------------------------------------------

// TestHandleChatSend_NoRunnerReturns503: if the chat runner is not wired,
// the handler refuses to render a fragment.
func TestHandleChatSend_NoRunnerReturns503(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	body := strings.NewReader("message=hello")
	req := httptest.NewRequest(http.MethodPost, "/chat/send", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleChatSend_NonPOSTReturns405 ensures GET (etc.) are rejected.
func TestHandleChatSend_NonPOSTReturns405(t *testing.T) {
	s := NewServer(3000)
	rec := httptest.NewRecorder()
	s.handleChatSend(rec, httptest.NewRequest(http.MethodGet, "/chat/send", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// TestHandleChatSend_EmptyMessageReturns400: a form with no message field
// or whitespace-only content is rejected.
func TestHandleChatSend_EmptyMessageReturns400(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})
	for _, bodyStr := range []string{"", "message=", "message=+++"} {
		rec := httptest.NewRecorder()
		body := strings.NewReader(bodyStr)
		req := httptest.NewRequest(http.MethodPost, "/chat/send", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.handleChatSend(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%q: expected 400, got %d", bodyStr, rec.Code)
		}
	}
}

// TestHandleChatSend_RendersFragment checks that a valid message produces
// the server-rendered user + assistant placeholder fragment.
func TestHandleChatSend_RendersFragment(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})

	form := url.Values{
		"message":    {"hello world"},
		"agent":      {"coder"},
		"session_id": {"s1"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type: want text/html, got %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="chat-msg is-user"`,
		`hello world`,
		`class="chat-msg is-assistant is-streaming"`,
		`id="chat-assistant"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
}

// TestHandleChatSend_IncludesHXTrigger verifies the HX-Trigger response
// header is set so the browser can start the streaming fetch.
func TestHandleChatSend_IncludesHXTrigger(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)

	if got := rec.Header().Get("HX-Trigger"); got != "chatSend" {
		t.Errorf("HX-Trigger: want chatSend, got %q", got)
	}
}

// TestHandleChatSend_OOBSessionElements verifies the fragment includes
// out-of-band swaps for session-id display and hidden form fields.
func TestHandleChatSend_OOBSessionElements(t *testing.T) {
	s := NewServer(3000)
	s.SetChatRunner(&stubChatRunner{})

	form := url.Values{
		"message":    {"hi"},
		"agent":      {"coder"},
		"session_id": {"s42"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`id="chat-session-id" hx-swap-oob="true"`,
		`>s42<`,
		`id="chat-session-input"`,
		`value="s42"`,
		`id="chat-agent-input"`,
		`value="coder"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
}
