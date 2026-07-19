package ui

import (
	"context"

	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
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

type blockingChatRunner struct {
	called  chan context.Context
	release chan struct{}
}

func (r *blockingChatRunner) Run(ctx context.Context, _, _ string, _ []ChatMessage) (string, <-chan ChatToken, error) {
	r.called <- ctx
	<-r.release
	ch := make(chan ChatToken, 1)
	ch <- ChatToken{Done: true}
	close(ch)
	return "", ch, nil
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
	setChatRunnerForTest(s, &stubChatRunner{})
	setAgentRegistryForTest(s, newStubRegistry(""))

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
	setChatRunnerForTest(s, &stubChatRunner{})
	setAgentRegistryForTest(s, newStubRegistry("",
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
	setChatRunnerForTest(s, &stubChatRunner{})
	setAgentRegistryForTest(s, newStubRegistry("coder",
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
	setChatRunnerForTest(s, &stubChatRunner{})
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
	setChatRunnerForTest(s, &stubChatRunner{})

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

func TestHandleChatSend_DoesNotRequireBrowserTrigger(t *testing.T) {
	s := NewServer(3000)
	setChatRunnerForTest(s, &stubChatRunner{})

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)

	if got := rec.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("HX-Trigger: want empty, got %q", got)
	}
}

func TestHandleChatSend_UsesRequestIndependentStreamContext(t *testing.T) {
	s := NewServer(3000)
	runner := &blockingChatRunner{called: make(chan context.Context, 1), release: make(chan struct{})}
	setChatRunnerForTest(s, runner)

	form := url.Values{"message": {"hi"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/send", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleChatSend(rec, req)
	defer close(runner.release)

	select {
	case ctx := <-runner.called:
		if err := ctx.Err(); err != nil {
			t.Fatalf("stream context was canceled with request: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner was not called")
	}
}

func TestStreamChatTokens_EscapesBroadcastTokenFrames(t *testing.T) {
	s := NewServer(3000)
	runner := &stubChatRunner{
		tokens: []ChatToken{
			{Content: "first\n<second>&"},
			{Done: true},
		},
	}

	ch := make(chan string, 4)
	s.chatSSEClients.Store(ch, "chat-a")
	defer s.chatSSEClients.Delete(ch)

	s.streamChatTokens(context.Background(), runner, "coder", "", "chat-a", []ChatMessage{{Role: "user", Content: "hi"}})

	var tokenFrame string
	select {
	case tokenFrame = <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for chat token frame")
	}
	for _, want := range []string{
		"event: chat-token\n",
		"data: first\n",
		"data: &lt;second&gt;&amp;\n",
	} {
		if !strings.Contains(tokenFrame, want) {
			t.Errorf("token frame missing %q, frame:\n%s", want, tokenFrame)
		}
	}
	if strings.Contains(tokenFrame, "\n<second>") {
		t.Errorf("token frame contains raw multiline payload: %q", tokenFrame)
	}
}

func TestBroadcastChatSSEWaitsForSubscriber(t *testing.T) {
	s := NewServer(3000)
	ch := make(chan string, 1)
	ch <- "existing"
	s.chatSSEClients.Store(ch, "chat-a")
	defer s.chatSSEClients.Delete(ch)

	done := make(chan struct{})
	go func() {
		s.broadcastChatSSE("chat-a", "event: chat-token\ndata: token\n\n")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("chat token frame was dropped instead of waiting for delivery")
	case <-time.After(50 * time.Millisecond):
	}
	if got := <-ch; got != "existing" {
		t.Fatalf("first frame = %q, want prefilled frame", got)
	}
	select {
	case got := <-ch:
		if !strings.Contains(got, "token") {
			t.Fatalf("chat frame = %q, want token payload", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for chat token frame")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast did not complete after subscriber drained")
	}
}

func TestBroadcastChatSSERoutesByStreamID(t *testing.T) {
	s := NewServer(3000)
	wantCh := make(chan string, 1)
	otherCh := make(chan string, 1)
	s.chatSSEClients.Store(wantCh, "chat-a")
	s.chatSSEClients.Store(otherCh, "chat-b")
	defer s.chatSSEClients.Delete(wantCh)
	defer s.chatSSEClients.Delete(otherCh)

	s.broadcastChatSSE("chat-a", "event: chat-token\ndata: hello\n\n")

	select {
	case got := <-wantCh:
		if !strings.Contains(got, "hello") {
			t.Fatalf("routed frame = %q, want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for routed chat frame")
	}
	select {
	case got := <-otherCh:
		t.Fatalf("other stream received frame %q", got)
	default:
	}
}

// TestHandleChatSend_OOBSessionElements verifies the fragment includes
// out-of-band swaps for session-id display and hidden form fields.
func TestHandleChatSend_OOBSessionElements(t *testing.T) {
	s := NewServer(3000)
	setChatRunnerForTest(s, &stubChatRunner{})

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
