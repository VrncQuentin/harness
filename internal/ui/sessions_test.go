package ui

import (
	"context"
	"encoding/json"
	"errors"
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
