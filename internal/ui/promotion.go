package ui

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Committer is the minimum surface the UI needs to commit memory repo files.
// Wired by the runtime from the go-git Repo.
type Committer interface {
	Commit(msg string, files []string) (string, error)
}

func (s *Server) SetCommitter(c Committer) {
	s.committerMu.Lock()
	s.committerData = c
	s.committerMu.Unlock()
}

func (s *Server) getCommitter() Committer {
	s.committerMu.RLock()
	defer s.committerMu.RUnlock()
	return s.committerData
}

// handlePromoteFact appends text to global/facts.md and commits it.
func (s *Server) handlePromoteFact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not available", http.StatusServiceUnavailable)
		return
	}
	c := s.getCommitter()
	if c == nil {
		http.Error(w, "committer not available", http.StatusServiceUnavailable)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	existing, _ := store.Read("global/facts.md")
	var builder strings.Builder
	builder.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		builder.WriteByte('\n')
	}
	builder.WriteString("\n")
	builder.WriteString(text)
	builder.WriteString("\n")
	if err := store.WriteFile("global/facts.md", []byte(builder.String())); err != nil {
		http.Error(w, fmt.Sprintf("write facts: %v", err), http.StatusInternalServerError)
		return
	}
	c.Commit("[type:fact] promote fact", []string{"global/facts.md"})
	http.Redirect(w, r, "/memory?promoted=1", http.StatusSeeOther)
}

// handleAppendNote appends text to an agent's notes.md and commits it.
func (s *Server) handleAppendNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.memoryStore()
	if store == nil {
		http.Error(w, "memory store not available", http.StatusServiceUnavailable)
		return
	}
	c := s.getCommitter()
	if c == nil {
		http.Error(w, "committer not available", http.StatusServiceUnavailable)
		return
	}
	agent := strings.TrimSpace(r.FormValue("agent"))
	if agent == "" || !validAgentName(agent) {
		http.Error(w, "valid agent name is required", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	notePath := fmt.Sprintf("agents/%s/notes.md", agent)
	existing, _ := store.Read(notePath)
	var builder strings.Builder
	builder.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		builder.WriteByte('\n')
	}
	builder.WriteString("\n")
	builder.WriteString(text)
	builder.WriteString("\n")
	if err := store.WriteFile(notePath, []byte(builder.String())); err != nil {
		http.Error(w, fmt.Sprintf("write note: %v", err), http.StatusInternalServerError)
		return
	}
	c.Commit(fmt.Sprintf("[agent:%s] [type:note] agent note", agent), []string{notePath})
	http.Redirect(w, r, "/memory?noted=1&agent="+url.QueryEscape(agent), http.StatusSeeOther)
}
