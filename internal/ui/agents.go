package ui

import (
	"net/http"
	"net/url"
	"strings"
)

// AgentInfo describes a single discovered agent. Persona and Notes
// hold the raw file contents (each may be empty when the corresponding
// file is missing) so the page can render them without the handler
// performing a second lookup.
type AgentInfo struct {
	Name        string
	PersonaPath string
	Persona     string
	NotesPath   string
	Notes       string
}

// AgentRegistry is the minimum surface the UI needs to list agents, fetch a
// persona, and manage which agent is currently active. The concrete
// implementation lives in `internal/agent` and is wired in main.go.
type AgentRegistry interface {
	List() ([]AgentInfo, error)
	Get(name string) (AgentInfo, error)
	Active() string
	SetActive(name string) error
	// Create makes a new agent named name in the memory repo. Name
	// validation is the registry's responsibility; the handler
	// reflects the returned error verbatim.
	Create(name string) error
	// WritePersona replaces the active agent's persona.md with body.
	// The agent must already exist; the handler surfaces the error
	// verbatim if it does not.
	WritePersona(name string, body []byte) error
	// WriteNotes replaces the active agent's notes.md with body.
	WriteNotes(name string, body []byte) error
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
	ActiveNotes   string
	Error         string
	// Configured is false when no registry has been wired up yet (typically
	// because memory.repo_path is unset or invalid). The template then
	// swaps the normal cards for a setup CTA.
	Configured bool
	// CreatedName is set after a successful create, surfaced to the user
	// as a one-shot "Created agent X" banner. Empty otherwise.
	CreatedName string
	// CreateErr is the validation/IO error from a failed create, rendered
	// next to the form so the user can correct and resubmit.
	CreateErr string
	// CreateName preserves the value the user typed when CreateErr is set
	// so they don't have to retype after a validation bounce.
	CreateName string
	// Saved drives a one-shot "saved" flash after a successful edit.
	// Values are "persona" or "notes"; empty otherwise.
	Saved string
	// SaveErr is set when an edit POST fails, rendered next to the
	// editor so the user can correct and resubmit.
	SaveErr string
}

// handleAgents renders the /agents page (GET only). Errors from the registry
// are surfaced inline so the user still sees whichever fields did resolve.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := s.buildAgentsView()
	if name := strings.TrimSpace(r.URL.Query().Get("created")); name != "" {
		data.CreatedName = name
	}
	switch r.URL.Query().Get("saved") {
	case "persona", "notes":
		data.Saved = r.URL.Query().Get("saved")
	}
	s.renderAgents(w, data)
}

// buildAgentsView assembles the registry-backed fields of agentsView. It
// is shared by handleAgents (GET render) and handleAgentsCreate (POST
// validation-error re-render) so both paths show the same list and
// active persona without duplicating the resolution logic.
func (s *Server) buildAgentsView() agentsView {
	data := agentsView{basePage: s.newBasePage("agents")}
	reg := s.agentRegistry()
	if reg == nil {
		return data
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
			data.ActiveNotes = info.Notes
		}
	}
	return data
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

// handleAgentsCreate handles POST /agents/create. On validation
// failure the page re-renders with the error inline and the entered
// name preserved (no PRG); on success it redirects to /agents with a
// ?created=<name> flash so the success banner survives reload without
// resubmitting on refresh.
func (s *Server) handleAgentsCreate(w http.ResponseWriter, r *http.Request) {
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
	if err := reg.Create(name); err != nil {
		data := s.buildAgentsView()
		data.CreateErr = err.Error()
		data.CreateName = name
		w.WriteHeader(http.StatusBadRequest)
		s.renderAgents(w, data)
		return
	}

	http.Redirect(w, r, "/agents?created="+url.QueryEscape(name), http.StatusSeeOther)
}

// editBodyMaxBytes caps the size of an editor POST body so a runaway
// paste cannot exhaust memory before we even parse the form.
const editBodyMaxBytes = 256 * 1024

// handleAgentsPersona writes the active agent's persona.md from the
// posted form. Edits are scoped to the active agent so the user
// cannot modify another agent's files via a stale form submission.
func (s *Server) handleAgentsPersona(w http.ResponseWriter, r *http.Request) {
	s.handleAgentsEdit(w, r, "persona", func(reg AgentRegistry, name string, body []byte) error {
		return reg.WritePersona(name, body)
	})
}

// handleAgentsNotes writes the active agent's notes.md from the
// posted form.
func (s *Server) handleAgentsNotes(w http.ResponseWriter, r *http.Request) {
	s.handleAgentsEdit(w, r, "notes", func(reg AgentRegistry, name string, body []byte) error {
		return reg.WriteNotes(name, body)
	})
}

// handleAgentsEdit is the shared body for handleAgentsPersona and
// handleAgentsNotes - they differ only in which writer they call and
// the saved-flash key returned in the redirect URL.
func (s *Server) handleAgentsEdit(
	w http.ResponseWriter,
	r *http.Request,
	kind string,
	write func(AgentRegistry, string, []byte) error,
) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reg := s.agentRegistry()
	if reg == nil {
		http.Error(w, "agent registry not configured", http.StatusServiceUnavailable)
		return
	}

	// Cap before ParseForm so a multi-megabyte paste is rejected at
	// the read layer rather than after we have copied it into memory.
	r.Body = http.MaxBytesReader(w, r.Body, editBodyMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := reg.Active()
	if name == "" {
		http.Error(w, "no active agent", http.StatusBadRequest)
		return
	}

	body := []byte(r.FormValue("body"))
	if err := write(reg, name, body); err != nil {
		data := s.buildAgentsView()
		data.SaveErr = err.Error()
		// Show what the user just typed so they don't lose work
		// after a write failure on the relevant editor.
		switch kind {
		case "persona":
			data.ActivePersona = string(body)
		case "notes":
			data.ActiveNotes = string(body)
		}
		w.WriteHeader(http.StatusBadRequest)
		s.renderAgents(w, data)
		return
	}

	http.Redirect(w, r, "/agents?saved="+kind, http.StatusSeeOther)
}
