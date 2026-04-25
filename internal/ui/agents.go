package ui

import (
	"net/http"
	"strings"
)

// AgentInfo describes a single discovered agent. Persona holds the raw file
// content (may be empty when the file is missing) so the page can render it
// without the handler performing a second lookup.
type AgentInfo struct {
	Name        string
	PersonaPath string
	Persona     string
}

// AgentRegistry is the minimum surface the UI needs to list agents, fetch a
// persona, and manage which agent is currently active. The concrete
// implementation lives in `internal/agent` and is wired in main.go.
type AgentRegistry interface {
	List() ([]AgentInfo, error)
	Get(name string) (AgentInfo, error)
	Active() string
	SetActive(name string) error
}

// SetAgentRegistry installs the registry used by the /agents page. Safe to
// leave unset; the page then renders a "memory repo not configured" card
// instead of a registry error.
func (s *Server) SetAgentRegistry(reg AgentRegistry) {
	s.agentRegMu.Lock()
	s.agentReg = reg
	s.agentRegMu.Unlock()
}

func (s *Server) agentRegistry() AgentRegistry {
	s.agentRegMu.RLock()
	defer s.agentRegMu.RUnlock()
	return s.agentReg
}

// agentsView is the template context for the /agents page.
type agentsView struct {
	basePage
	Agents        []AgentInfo
	Active        string
	ActivePersona string
	Error         string
	// Configured is false when no registry has been wired up yet (typically
	// because memory.repo_path is unset or invalid). The template then
	// swaps the normal cards for a setup CTA.
	Configured bool
}

// handleAgents renders the /agents page (GET only). Errors from the registry
// are surfaced inline so the user still sees whichever fields did resolve.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := agentsView{basePage: s.newBasePage("agents")}
	reg := s.agentRegistry()
	if reg == nil {
		s.renderAgents(w, data)
		return
	}
	data.Configured = true

	list, err := reg.List()
	if err != nil {
		data.Error = err.Error()
	}
	data.Agents = list

	active := reg.Active()
	data.Active = active
	if active != "" {
		info, err := reg.Get(active)
		if err != nil {
			// Keep the previous error (if any) visible; otherwise surface
			// this one. Listing succeeding but Get failing is a weird
			// state worth telling the user about.
			if data.Error == "" {
				data.Error = err.Error()
			}
		} else {
			data.ActivePersona = info.Persona
		}
	}

	s.renderAgents(w, data)
}

func (s *Server) renderAgents(w http.ResponseWriter, data agentsView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.agentsTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// handleAgentsActive switches (or clears) the active agent. POST only; an
// empty form value clears the active selection so the UI can render "no
// agent selected" without requiring a sentinel on the registry side.
func (s *Server) handleAgentsActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reg := s.agentRegistry()
	if reg == nil {
		http.Error(w, "agent registry not configured", http.StatusServiceUnavailable)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name != "" {
		// Validate the name refers to a known agent before switching so a
		// typo or stale form value produces a 400 rather than a broken
		// active pointer.
		if _, err := reg.Get(name); err != nil {
			http.Error(w, "unknown agent: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := reg.SetActive(name); err != nil {
		http.Error(w, "could not set active agent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/agents", http.StatusSeeOther)
}
