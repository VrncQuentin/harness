package ui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/VrncQuentin/harness/internal/project"
)

// defaultRecentSessions is how many saved sessions the project view page
// (and, via config, the sidebar) lists per project. PR 2 makes the sidebar
// count configurable; the page uses this default for now.
const defaultRecentSessions = 5

// projectViewData is the template context for /projects/view.
type projectViewData struct {
	basePage
	Project        *projectRow
	RecentSessions []SessionRecord
	SessionsErr    string
	RecentLimit    int
}

// handleProjectView renders one project's page: its identity, activation
// control, config link, and the project's recent saved sessions. It is the
// destination of a sidebar project click, replacing the /projects
// management page.
func (s *Server) handleProjectView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap, release := s.acquireSnapshot()
	defer release()

	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if err := project.ValidateSlug(slug); err != nil {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}

	store := s.getProjectStore()
	if store == nil {
		http.Error(w, "project store not available", http.StatusServiceUnavailable)
		return
	}
	p, err := store.Get(slug)
	if err != nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	row := projectRow{
		Slug:           p.Slug,
		DisplayName:    p.DisplayName,
		Hidden:         p.Hidden,
		IsGlobal:       p.Slug == project.GlobalSlug,
		IsActive:       p.Slug == s.activeProjectSlug(),
		MemoryRepoPath: p.MemoryRepoPath,
	}

	data := projectViewData{
		basePage:    s.newBasePage("projects"),
		Project:     &row,
		RecentLimit: s.sidebarRecentSessionsCount(),
	}
	if ps := snap.ProjectSessions; ps != nil && data.RecentLimit > 0 {
		recs, err := ps.Recent(slug, data.RecentLimit)
		if err != nil {
			data.SessionsErr = err.Error()
		} else {
			data.RecentSessions = recs
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.projectViewTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// handleProjectChat starts a new session for a project from its view page.
// Chat runs against the active project's context, so this activates the
// project first (the session's project becomes the active one), then hands
// the user off to /chat with their opening message prefilled. The session
// itself is created by the chat page on the first send.
func (s *Server) handleProjectChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	if err := project.ValidateSlug(slug); err != nil {
		http.Error(w, "invalid project", http.StatusBadRequest)
		return
	}
	if err := s.activateProject(slug); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.refreshProjectNav()

	target := "/chat"
	if msg := strings.TrimSpace(r.FormValue("message")); msg != "" {
		target += "?message=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
