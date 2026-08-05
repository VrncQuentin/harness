package ui

import (
	"net/http"
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
		RecentLimit: defaultRecentSessions,
	}
	if ps := snap.ProjectSessions; ps != nil {
		recs, err := ps.Recent(slug, defaultRecentSessions)
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
