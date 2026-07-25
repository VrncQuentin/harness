package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/VrncQuentin/harness/internal/memory"
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

func (s *Server) getCommitter() Committer {
	return s.depsSnapshot().committer
}

func (s *Server) getDedupChecker() DedupChecker {
	return s.depsSnapshot().dedup
}

func (s *Server) getPromotionDedupThreshold() float64 {
	return s.depsSnapshot().promotionDedupThreshold
}

// handlePromoteFact appends text to facts.md in the active project memory repo and commits it.
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

	svc := memory.PromotionService{Store: store, Committer: c}
	if err := svc.PromoteFact(text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	svc := memory.PromotionService{Store: store, Committer: c}
	if err := svc.AppendAgentNote(agent, text); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/memory?noted=1&agent="+url.QueryEscape(agent), http.StatusSeeOther)
}
