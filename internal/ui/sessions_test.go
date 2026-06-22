package ui

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
	if s.saveRes.ID == "" {
		s.saveRes.ID = id
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

func (s *stubSessionStore) Resume(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resumeCalls++
	return s.resumeErr
}

func TestHandleChatSave_NoStoreReturns503(t *testing.T) {
	srv := NewServer(0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(`{"session_id":"x"}`))
	srv.handleChatSave(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", rec.Code)
	}
}

func TestHandleChatSave_HappyPath(t *testing.T) {
	store := &stubSessionStore{
		saveRes: SessionSaveResult{
			ID:          "abc",
			EpisodePath: "projects/global/episodes/coder/abc.md",
			Summary:     "all good",
			SavedAt:     time.Date(2026, 4, 26, 22, 0, 0, 0, time.UTC),
			SaveSeq:     1,
		},
	}
	srv := NewServer(0)
	srv.SetSessionStore(store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(`{"session_id":"abc"}`))
	srv.handleChatSave(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var got SessionSaveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "abc" {
		t.Errorf("id: want abc, got %q", got.ID)
	}
	if store.saveID != "abc" || store.saveCalls != 1 {
		t.Errorf("store.Save called=%d id=%q", store.saveCalls, store.saveID)
	}
}

func TestHandleChatSave_EmptyBodyReturns400(t *testing.T) {
	srv := NewServer(0)
	srv.SetSessionStore(&stubSessionStore{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(`{}`))
	srv.handleChatSave(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
}

func TestHandleChatSessionResume_Happy(t *testing.T) {
	want := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hey"},
	}
	store := &stubSessionStore{convResult: want}
	srv := NewServer(0)
	srv.SetSessionStore(store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	srv.handleChatSessionResume(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	var resp chatSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(resp.Messages))
	}
	if store.resumeCalls != 1 {
		t.Errorf("Resume should be called once, got %d", store.resumeCalls)
	}
}

func TestHandleChatSessionResume_ConversationLost404(t *testing.T) {
	store := &stubSessionStore{convErr: ErrSessionConversationLost}
	srv := NewServer(0)
	srv.SetSessionStore(store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	srv.handleChatSessionResume(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
}

func TestHandleChatSessionResume_MissingArgs400(t *testing.T) {
	srv := NewServer(0)
	srv.SetSessionStore(&stubSessionStore{})
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
	srv.SetSessionStore(store)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat/save", strings.NewReader(`{"session_id":"x"}`))
	srv.handleChatSave(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", rec.Code)
	}
}

// TestHandleChatSessionResume_HXRequestReturnsHTMLFragment verifies that
// when htmx drives the request (HX-Request: true), the response is an
// HTML fragment with server-rendered message divs, an out-of-band
// session-id update, and a hidden state span carrying the JSON data for
// the browser's JS to re-sync its messages array.
func TestHandleChatSessionResume_HXRequestReturnsHTMLFragment(t *testing.T) {
	want := []ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	store := &stubSessionStore{convResult: want}
	srv := NewServer(0)
	srv.SetSessionStore(store)

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

	// Hidden state span with data attributes for JS sync.
	if !strings.Contains(body, `id="chat-state" hx-swap-oob="true"`) {
		t.Errorf("fragment missing oob chat-state span")
	}
	if !strings.Contains(body, `data-session-id`) {
		t.Errorf("fragment missing data-session-id attribute")
	}
	if !strings.Contains(body, `data-messages`) {
		t.Errorf("fragment missing data-messages attribute")
	}

	// Verify the messages JSON in the data attribute is parseable.
	idx := strings.Index(body, `data-messages=`)
	if idx < 0 {
		t.Fatal("data-messages attribute not found")
	}
	// Extract the value between data-messages=" and the closing ".
	// The template HTML-escapes the JSON; UnescapeString recovers it.
	rest := body[idx+len(`data-messages=`):]
	rest = strings.TrimPrefix(rest, `"`)
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatal("data-messages closing quote not found")
	}
	dataVal := html.UnescapeString(rest[:end])
	var msgs []ChatMessage
	if err := json.Unmarshal([]byte(dataVal), &msgs); err != nil {
		t.Fatalf("data-messages JSON not parseable: %v\nvalue: %s", err, dataVal)
	}
	if len(msgs) != 2 || msgs[0].Content != "hello" {
		t.Errorf("data-messages content mismatch: %+v", msgs)
	}
}

// TestHandleChatSessionResume_HXRequestEmptyConversation verifies the
// fragment when the conversation has zero messages.
func TestHandleChatSessionResume_HXRequestEmptyConversation(t *testing.T) {
	store := &stubSessionStore{convResult: nil}
	srv := NewServer(0)
	srv.SetSessionStore(store)

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

// TestHandleChatSessionResume_JSONBackwardCompat verifies that without
// HX-Request, the handler still returns the existing JSON response.
func TestHandleChatSessionResume_JSONBackwardCompat(t *testing.T) {
	want := []ChatMessage{
		{Role: "user", Content: "hi"},
	}
	store := &stubSessionStore{convResult: want}
	srv := NewServer(0)
	srv.SetSessionStore(store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	// No HX-Request header.
	srv.handleChatSessionResume(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type: want application/json, got %q", ct)
	}
	var resp chatSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "abc" || resp.Agent != "coder" || len(resp.Messages) != 1 {
		t.Errorf("response mismatch: %+v", resp)
	}
}

// TestHandleChatSessionResume_HXRequestConversationLost verifies that a
// 404 conversation-lost error is returned even when HX-Request is set.
func TestHandleChatSessionResume_HXRequestConversationLost(t *testing.T) {
	store := &stubSessionStore{convErr: ErrSessionConversationLost}
	srv := NewServer(0)
	srv.SetSessionStore(store)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat/session?agent=coder&id=abc", nil)
	req.Header.Set("HX-Request", "true")
	srv.handleChatSessionResume(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d (body %s)", rec.Code, rec.Body.String())
	}
}
