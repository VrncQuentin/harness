package ui

import (
	"net/http"
	"net/url"
	"strings"
)

// AgentInfo describes a single discovered agent. Persona, Rules, and
// Notes hold the raw file contents (each may be empty when the
// corresponding file is missing) so the page can render them without
// the handler performing a second lookup.
type AgentInfo struct {
	Name        string
	PersonaPath string
	Persona     string
	RulesPath   string
	Rules       string
	NotesPath   string
	Notes       string
	// Origin is "global", "extends-global", or "project-only", indicating
	// whether the agent is defined globally, overridden by the active
	// project, or local to the active project.
	Origin string
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
	// WriteRules replaces the active agent's rules.md with body.
	WriteRules(name string, body []byte) error
	// WriteNotes replaces the active agent's notes.md with body.
	WriteNotes(name string, body []byte) error
	// Delete removes the agent named name. Implementations are
	// responsible for clearing the active selection if it pointed at
	// the removed agent so the UI never has to coordinate the two
	// state changes itself.
	Delete(name string) error
}

// agentsView is the template context for the /agents page.
type agentsView struct {
	basePage
	Agents []AgentInfo
	Active string
	Error  string
	// Configured is false when no registry has been wired up yet (typically
	// because the active memory repo is unavailable). The template then
	// swaps the normal cards for a setup CTA.
	Configured bool
	// CreatedName is set after a successful create, surfaced to the user
	// as a one-shot "Created agent X" banner. Empty otherwise.
	CreatedName string
	// DeletedName is set after a successful delete, surfaced as a
	// one-shot "Deleted agent X" banner. Empty otherwise.
	DeletedName string
	// CreateErr is the validation/IO error from a failed create, rendered
	// next to the form so the user can correct and resubmit.
	CreateErr string
	// CreateName preserves the value the user typed when CreateErr is set
	// so they don't have to retype after a validation bounce.
	CreateName string
	// Saved drives a one-shot "saved" flash after a successful edit.
	// Values are "persona", "rules", or "notes"; empty otherwise.
	Saved string
	// EditName is the agent currently in edit mode (from the ?edit=
	// query param or set by an edit POST that failed). Empty means
	// every card is in view mode.
	EditName string
	// SaveErr is set when an edit POST fails, rendered above the
	// editor so the user can correct and resubmit. Pairs with
	// EditName so the failed agent's card stays in edit mode.
	SaveErr string
}

// handleAgents renders the /agents page (GET only). Errors from the registry
// are surfaced inline so the user still sees whichever fields did resolve.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snap, release := s.acquireSnapshot()
	defer release()
	data := s.buildAgentsView(snap.AgentRegistry, snap.ActiveAgent)
	if name := strings.TrimSpace(r.URL.Query().Get("created")); name != "" {
		data.CreatedName = name
	}
	if name := strings.TrimSpace(r.URL.Query().Get("deleted")); name != "" {
		data.DeletedName = name
	}
	switch r.URL.Query().Get("saved") {
	case "persona", "rules", "notes":
		data.Saved = r.URL.Query().Get("saved")
	}
	if edit := strings.TrimSpace(r.URL.Query().Get("edit")); edit != "" {
		// Only enter edit mode if the named agent actually exists -
		// a stale or hand-edited URL silently falls back to view mode
		// rather than rendering an empty editor for a ghost agent.
		for _, a := range data.Agents {
			if a.Name == edit {
				data.EditName = edit
				break
			}
		}
	}
	s.renderAgents(w, data)
}

// buildAgentsView assembles the registry-backed fields of agentsView. It
// is shared by handleAgents (GET render) and the create/edit POST
// re-render paths so all of them show the same list without
// duplicating the resolution logic. Persona/rules/notes are sourced
// from the registry's List() output, which the adapter hydrates with
// file contents - that lets each card render inline without an extra
// Get() per agent.
//
// active is the acquisition-scoped active agent from the snapshot. The page
// marks it as the current selection; using the captured value rather than
// re-reading the registry's live selection keeps the marker consistent with
// the listed generation.
func (s *Server) buildAgentsView(reg AgentRegistry, active string) agentsView {
	data := agentsView{basePage: s.newBasePage("agents")}
	if reg == nil {
		return data
	}
	data.Configured = true

	list, err := reg.List()
	if err != nil {
		data.Error = err.Error()
	}
	data.Agents = list
	data.Active = active
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

	snap, release := s.acquireSnapshot()
	defer release()
	reg := snap.AgentRegistry
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

	snap, release := s.acquireSnapshot()
	defer release()
	reg := snap.AgentRegistry
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
		data := s.buildAgentsView(reg, snap.ActiveAgent)
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

// handleAgentsRules writes the active agent's rules.md from the
// posted form.
func (s *Server) handleAgentsRules(w http.ResponseWriter, r *http.Request) {
	s.handleAgentsEdit(w, r, "rules", func(reg AgentRegistry, name string, body []byte) error {
		return reg.WriteRules(name, body)
	})
}

// handleAgentsNotes writes the active agent's notes.md from the
// posted form.
func (s *Server) handleAgentsNotes(w http.ResponseWriter, r *http.Request) {
	s.handleAgentsEdit(w, r, "notes", func(reg AgentRegistry, name string, body []byte) error {
		return reg.WriteNotes(name, body)
	})
}

// handleAgentsEdit is the shared body for handleAgentsPersona,
// handleAgentsRules, and handleAgentsNotes - they differ only in
// which writer they call and the saved-flash key returned in the
// redirect URL. The agent name is read from the form so the page can
// edit any agent (not just the active one); each card embeds its own
// name as a hidden field.
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

	snap, release := s.acquireSnapshot()
	defer release()
	reg := snap.AgentRegistry
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

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "missing agent name", http.StatusBadRequest)
		return
	}
	if _, err := reg.Get(name); err != nil {
		http.Error(w, "unknown agent: "+err.Error(), http.StatusBadRequest)
		return
	}

	body := []byte(r.FormValue("body"))
	if err := write(reg, name, body); err != nil {
		data := s.buildAgentsView(reg, snap.ActiveAgent)
		data.SaveErr = err.Error()
		data.EditName = name
		// Show what the user just typed so they don't lose work
		// after a write failure on the relevant editor.
		for i := range data.Agents {
			if data.Agents[i].Name != name {
				continue
			}
			switch kind {
			case "persona":
				data.Agents[i].Persona = string(body)
			case "rules":
				data.Agents[i].Rules = string(body)
			case "notes":
				data.Agents[i].Notes = string(body)
			}
			break
		}
		w.WriteHeader(http.StatusBadRequest)
		s.renderAgents(w, data)
		return
	}

	// Stay in edit mode after save so the user can keep tweaking
	// without re-clicking Edit on every save.
	http.Redirect(w, r, "/agents?edit="+url.QueryEscape(name)+"&saved="+kind, http.StatusSeeOther)
}

// handleAgentsDelete removes an agent. The page is the only entry
// point so we treat any GET as 405; the confirmation popup posts the
// agent name from a hidden field. On success we PRG to /agents with
// a ?deleted= flash so the success banner survives a reload without
// resubmitting on refresh.
func (s *Server) handleAgentsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snap, release := s.acquireSnapshot()
	defer release()
	reg := snap.AgentRegistry
	if reg == nil {
		http.Error(w, "agent registry not configured", http.StatusServiceUnavailable)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "missing agent name", http.StatusBadRequest)
		return
	}
	if _, err := reg.Get(name); err != nil {
		http.Error(w, "unknown agent: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := reg.Delete(name); err != nil {
		http.Error(w, "could not delete agent: "+err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/agents?deleted="+url.QueryEscape(name), http.StatusSeeOther)
}
