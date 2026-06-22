package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// SessionStore is the surface the chat page uses to save, list, and
// resume sessions. The concrete implementation lives in
// internal/runtime; this interface keeps the ui package free of
// session/git/inference imports.
type SessionStore interface {
	// Save persists the session identified by id, regenerates the
	// summary, commits the .md, and appends a record to sessions.jsonl.
	Save(ctx context.Context, id string) (SessionSaveResult, error)
	// Records returns the most recent saved sessions for agent. An
	// empty agent uses the active agent.
	Records(agent string) ([]SessionRecord, error)
	// Conversation hydrates the .json sidecar for one session record.
	// Returns ErrSessionConversationLost when the sidecar is missing
	// (typical on a fresh clone) so the UI can disable the resume row.
	Conversation(agent, id string) ([]ChatMessage, error)
	// Resume registers id with the manager so the next /chat/stream
	// call appends onto the resumed conversation.
	Resume(id string) error
}

// SessionSaveResult is the small slice of the manager's SaveResult the
// UI surfaces back to the browser.
type SessionSaveResult struct {
	ID          string    `json:"id"`
	EpisodePath string    `json:"episode_path"`
	Summary     string    `json:"summary"`
	SavedAt     time.Time `json:"saved_at"`
	SaveSeq     int       `json:"save_seq"`
}

// SessionRecord is one saved-session entry rendered by the resume
// picker. Mirrors session.Record verbatim minus the project field
// (hardcoded to "global" in M3).
type SessionRecord struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	StartedAt   time.Time `json:"started_at"`
	SavedAt     time.Time `json:"saved_at"`
	SaveSeq     int       `json:"save_seq"`
	EpisodePath string    `json:"episode_path"`
}

// SetSessionStore installs the store used by /chat/save and friends.
// Pass nil to detach (e.g. when the memory repo is invalidated); the
// handlers then return 503 until a valid store is wired back in.
func (s *Server) SetSessionStore(store SessionStore) {
	s.sessionStoreMu.Lock()
	s.sessionStore = store
	s.sessionStoreMu.Unlock()
}

func (s *Server) getSessionStore() SessionStore {
	s.sessionStoreMu.RLock()
	defer s.sessionStoreMu.RUnlock()
	return s.sessionStore
}

// chatSaveRequest is the JSON body of POST /chat/save. SessionID is required.
type chatSaveRequest struct {
	SessionID string `json:"session_id"`
}

// chatSaveMaxBytes caps the save request body. The body is tiny by
// design (just the session id), so a generous limit still rejects
// runaway payloads from a misbehaving extension.
const chatSaveMaxBytes = 8 * 1024

// handleChatSave persists the live session and returns the SaveResult
// JSON. Used by the explicit Save button on /chat.
func (s *Server) handleChatSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.getSessionStore()
	if store == nil {
		writeChatJSONError(w, http.StatusServiceUnavailable, "session manager not available")
		return
	}
	id, ok := decodeSaveRequest(w, r)
	if !ok {
		return
	}
	res, err := store.Save(r.Context(), id)
	if err != nil {
		writeChatJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// decodeSaveRequest reads the small JSON body and pulls out the
// session id, writing a 400 on parse failure. Returns the id and true
// on success.
func decodeSaveRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, chatSaveMaxBytes)
	var req chatSaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeChatJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return "", false
	}
	id := strings.TrimSpace(req.SessionID)
	if id == "" {
		writeChatJSONError(w, http.StatusBadRequest, "session_id is required")
		return "", false
	}
	return id, true
}

// chatSessionResponse is the JSON body of GET /chat/session.
type chatSessionResponse struct {
	ID       string        `json:"id"`
	Agent    string        `json:"agent"`
	Messages []ChatMessage `json:"messages"`
}

// chatSessionView is the template data for the chat-transcript-fragment
// partial. It renders the transcript as HTML fragments for htmx-driven
// resume, replacing the browser's JS transcript rebuild.
type chatSessionView struct {
	SessionID    string
	Messages     []ChatMessage
	MessagesJSON string
}

// handleChatSessionResume hydrates one session's conversation from the
// .json sidecar so the browser can replace its transcript. When the
// request carries the HX-Request header (htmx), it returns an HTML
// fragment rendered server-side instead of JSON, moving transcript
// state ownership to the server.
func (s *Server) handleChatSessionResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.getSessionStore()
	if store == nil {
		writeChatJSONError(w, http.StatusServiceUnavailable, "session manager not available")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if id == "" || agent == "" {
		writeChatJSONError(w, http.StatusBadRequest, "id and agent are required")
		return
	}
	msgs, err := store.Conversation(agent, id)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionConversationLost):
			writeChatJSONError(w, http.StatusNotFound, err.Error())
		default:
			writeChatJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := store.Resume(id); err != nil {
		switch {
		case errors.Is(err, ErrSessionConversationLost):
			writeChatJSONError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrSessionUnknown):
			writeChatJSONError(w, http.StatusNotFound, err.Error())
		default:
			writeChatJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// When driven by htmx, return a server-rendered transcript fragment
	// so the browser no longer needs JS to rebuild the conversation.
	if r.Header.Get("HX-Request") == "true" {
		jsonBytes, err := json.Marshal(msgs)
		if err != nil {
			writeChatJSONError(w, http.StatusInternalServerError, "marshal messages: "+err.Error())
			return
		}
		view := chatSessionView{
			SessionID:    id,
			Messages:     msgs,
			MessagesJSON: string(jsonBytes),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.chatTmpl.ExecuteTemplate(w, "chat-transcript-fragment", view); err != nil {
			http.Error(w, "template error", http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(chatSessionResponse{
		ID:       id,
		Agent:    agent,
		Messages: msgs,
	})
}
