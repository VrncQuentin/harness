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

// stubSessionStore is a test double for the SessionStore interface.
type stubSessionStore struct {
	mu        sync.Mutex
	saveCalls int
	saveID    string
	saveRes   SessionSaveResult
	saveErr   error

	records    []SessionRecord
	recordsErr error
	gotAgent   string

	convCalls   int
	convAgent   string
	convID      string
	convResult  []ChatMessage
	convErr     error
	liveResult  []ChatMessage
	liveErr     error
	resumeErr   error
	resumeCalls int
}

func (s *stubSessionStore) Save(_ context.Context, id string) (SessionSaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCalls++
	s.saveID = id
	if s.saveErr != nil {
		return SessionSaveResult{}, s.saveErr
	}
	return s.saveRes, nil
}

func (s *stubSessionStore) Records(agent string) ([]SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotAgent = agent
	return s.records, s.recordsErr
}

func (s *stubSessionStore) Conversation(agent, id string) ([]ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.convCalls++
	s.convAgent = agent
	s.convID = id
	if s.convErr != nil {
		return nil, s.convErr
	}
	return s.convResult, nil
}

func (s *stubSessionStore) LiveConversation(id string) ([]ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.liveErr != nil {
		return nil, s.liveErr
	}
	return s.liveResult, nil
}

func (s *stubSessionStore) Resume(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumeCalls++
	return s.resumeErr
}

func TestHandleChatSessionResume_ConversationLost404(t *testing.T) {
	store := &stubSessionStore{convErr: ErrSessionConversationLost}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	srv.handleChatSessionResume(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
}

func TestHandleChatSessionResume_MissingArgs400(t *testing.T) {
	srv := NewServer(0)
	setSessionStoreForTest(srv, &stubSessionStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?id=abc", nil)
	srv.handleChatSessionResume(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
}

func TestHandleChatSave_StoreErrorIs500(t *testing.T) {
	store := &stubSessionStore{saveErr: errors.New("boom")}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)
	form := url.Values{"session_id": {"x"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleChatSave(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rec.Code)
	}
}

// TestHandleChatSessionResume_ReturnsHTMLFragment verifies that the response
// is an HTML fragment with server-rendered message divs and an out-of-band
// session-id update.
func TestHandleChatSessionResume_ReturnsHTMLFragment(t *testing.T) {
	want := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	store := &stubSessionStore{convResult: want}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	req.Header.Set("HX-Request", "true")
	srv.handleChatSessionResume(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type: want text/html, got %q", ct)
	}

	body := rec.Body.String()

	// Rendered message divs.
	for _, expected := range []string{
		`class="chat-msg is-user"`,
		`hello`,
		`class="chat-msg is-assistant"`,
		`hi there`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("fragment missing %q, body:\n%s", expected, body)
		}
	}

	// Out-of-band session-id update.
	if !strings.Contains(body, `id="chat-session-id" hx-swap-oob="true"`) {
		t.Errorf("fragment missing oob session-id swap")
	}
	if !strings.Contains(body, `>abc<`) {
		t.Errorf("fragment missing session-id value")
	}

	if strings.Contains(body, `id="chat-state"`) || strings.Contains(body, `data-messages`) {
		t.Errorf("fragment should not render hidden chat-state JSON, body:\n%s", body)
	}
}

// TestHandleChatSessionResume_EmptyConversationFragment verifies the
// fragment when the conversation has zero messages.
func TestHandleChatSessionResume_EmptyConversationFragment(t *testing.T) {
	store := &stubSessionStore{convResult: nil}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	req.Header.Set("HX-Request", "true")
	srv.handleChatSessionResume(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="chat-empty"`) {
		t.Errorf("expected empty placeholder for 0 messages")
	}
}

// TestHandleChatSessionResume_ConversationLostWithFragmentRequest verifies that a
// 404 conversation-lost error is returned.
func TestHandleChatSessionResume_ConversationLostWithFragmentRequest(t *testing.T) {
	store := &stubSessionStore{convErr: ErrSessionConversationLost}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	req.Header.Set("HX-Request", "true")
	srv.handleChatSessionResume(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleChatSave_ReturnsHTML verifies the save flow
// returns an HTML confirmation fragment.
func TestHandleChatSave_ReturnsHTML(t *testing.T) {
	store := &stubSessionStore{
		saveRes: SessionSaveResult{
			SaveSeq: 1,
		},
	}
	srv := NewServer(0)
	setSessionStoreForTest(srv, store)

	form := url.Values{"session_id": {"abc"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	srv.handleChatSave(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type: want text/html, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Saved session abc (seq 1)") {
		t.Errorf("expected save confirmation, got: %s", body)
	}
}

// TestHandleChatSave_MissingIDReturns400 verifies validation
// when the form lacks session_id.
func TestHandleChatSave_MissingIDReturns400(t *testing.T) {
	srv := NewServer(0)
	setSessionStoreForTest(srv, &stubSessionStore{})

	form := url.Values{"session_id": {""}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	srv.handleChatSave(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
}

// TestHandleChatSave_NoStoreReturns503 verifies the 503 when
// no session store is wired.
func TestHandleChatSave_NoStoreReturns503(t *testing.T) {
	srv := NewServer(0)

	form := url.Values{"session_id": {"x"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	srv.handleChatSave(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", rec.Code)
	}
}
