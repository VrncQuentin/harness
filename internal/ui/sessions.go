package ui

import (
	"context"
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
	// LiveConversation returns the in-memory conversation for an active
	// session, if it exists in this process.
	LiveConversation(id string) ([]ChatMessage, error)
	// Resume registers id with the manager so the next /chat/send
	// call appends onto the resumed conversation.
	Resume(id string) error
}

// SessionSaveResult is the small slice of the manager's SaveResult the
// UI surfaces back to the browser.
type SessionSaveResult struct {
	SaveSeq int `json:"save_seq"`
}

// SessionRecord is one saved-session entry rendered by the resume picker.
// It mirrors the session package record shape minus fields the UI does not
// display.
type SessionRecord struct {
	ID      string    `json:"id"`
	Agent   string    `json:"agent"`
	SavedAt time.Time `json:"saved_at"`
	SaveSeq int       `json:"save_seq"`
}

// ProjectSessions lists saved sessions for a project, newest-first. It is
// project-scoped (the runtime opens the target project's memory repo on
// demand), unlike SessionStore which is bound to the active project's
// manager.
type ProjectSessions interface {
	Recent(slug string, limit int) ([]SessionRecord, error)
}

// chatSaveView is the template data for the chat-save-fragment partial.
type chatSaveView struct {
	SessionID string
	SaveSeq   int
}

// chatSaveMaxBytes caps the save request body. The body is tiny by
// design (just the session id), so a generous limit still rejects
// runaway payloads from a misbehaving extension.
const chatSaveMaxBytes = 8 * 1024

// handleChatSave persists the live session and returns an HTML fragment with a
// confirmation message for the htmx chat page.
func (s *Server) handleChatSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, release := s.acquireSnapshot()
	defer release()
	store := snap.SessionStore
	if store == nil {
		http.Error(w, "session manager not available", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, chatSaveMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.FormValue("session_id"))
	if id == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}

	res, err := store.Save(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.chatTmpl.ExecuteTemplate(w, "chat-save-fragment", chatSaveView{
		SessionID: id,
		SaveSeq:   res.SaveSeq,
	}); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// chatSessionView is the template data for the chat-transcript-fragment
// partial. It renders the transcript as HTML fragments for htmx-driven
// resume, replacing the browser's JS transcript rebuild.
type chatSessionView struct {
	SessionID string
	Messages  []ChatMessage
}

// handleChatSessionResume hydrates one session's conversation from the .json
// sidecar and returns a server-rendered transcript fragment for htmx.
func (s *Server) handleChatSessionResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, release := s.acquireSnapshot()
	defer release()
	store := snap.SessionStore
	if store == nil {
		http.Error(w, "session manager not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if id == "" || agent == "" {
		http.Error(w, "id and agent are required", http.StatusBadRequest)
		return
	}
	msgs, err := store.Conversation(agent, id)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionConversationLost):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if err := store.Resume(id); err != nil {
		switch {
		case errors.Is(err, ErrSessionConversationLost):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrSessionUnknown):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	view := chatSessionView{
		SessionID: id,
		Messages:  msgs,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.chatTmpl.ExecuteTemplate(w, "chat-transcript-fragment", view); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
