package ui

import (
	"bytes"
	"context"
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

// DedupChecker checks whether promoting a fact would create a near-duplicate
// of an existing fact. Threshold is the cosine similarity above which the
// new fact is considered a duplicate and blocked.
type DedupChecker interface {
	CheckSimilar(ctx context.Context, text string, threshold float64) (blocked bool, similarFact string, score float64, err error)
}

func (s *Server) SetCommitter(c Committer) {
	s.updateDeps(func(d *uiDeps) { d.committer = c })
}

func (s *Server) getCommitter() Committer {
	return s.depsSnapshot().committer
}

func (s *Server) SetDedupChecker(dc DedupChecker) {
	s.updateDeps(func(d *uiDeps) { d.dedup = dc })
}

func (s *Server) getDedupChecker() DedupChecker {
	return s.depsSnapshot().dedup
}

func (s *Server) SetPromotionDedupThreshold(t float64) {
	s.updateDeps(func(d *uiDeps) { d.promotionDedupThreshold = t })
}

func (s *Server) getPromotionDedupThreshold() float64 {
	return s.depsSnapshot().promotionDedupThreshold
}

// handlePromoteFact appends text to global/facts.md and commits it.
// When a DedupChecker and non-zero threshold are available, the handler
// checks for near-duplicate facts before writing and redirects with a
// dedup-blocked flash message if one is found.
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

	// Dedup check (optional — skipped when checker or threshold is absent).
	if checker := s.getDedupChecker(); checker != nil {
		threshold := s.getPromotionDedupThreshold()
		if threshold > 0 {
			blocked, similar, score, err := checker.CheckSimilar(r.Context(), text, threshold)
			if err != nil {
				http.Error(w, fmt.Sprintf("dedup check failed: %v", err), http.StatusInternalServerError)
				return
			}
			if blocked {
				u := fmt.Sprintf("/memory?dedup=1&similar=%s&score=%.2f", url.QueryEscape(similar), score)
				http.Redirect(w, r, u, http.StatusSeeOther)
				return
			}
		}
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
	if _, err := c.Commit("[type:fact] promote fact", []string{"global/facts.md"}); err != nil {
		http.Error(w, fmt.Sprintf("commit fact: %v", err), http.StatusInternalServerError)
		return
	}
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
	if _, err := c.Commit(fmt.Sprintf("[agent:%s] [type:note] agent note", agent), []string{notePath}); err != nil {
		http.Error(w, fmt.Sprintf("commit note: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/memory?noted=1&agent="+url.QueryEscape(agent), http.StatusSeeOther)
}
