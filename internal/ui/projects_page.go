package ui

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VrncQuentin/harness/internal/memory"
	"github.com/VrncQuentin/harness/internal/project"
)

type projectRow struct {
	Slug           string
	DisplayName    string
	Hidden         bool
	IsGlobal       bool
	IsActive       bool
	DirectoryCount int
	MemoryRepoPath string
	ModelPath      string
	ModelBinary    string
	ModelCtxSize   string
	ModelGPULayers string
	ModelNParallel string
}

type projectsView struct {
	basePage
	Projects    []projectRow
	EditProject *projectRow
	Error       string
	Flash       string
	ShowHidden  bool
}

func (s *Server) renderProjects(w http.ResponseWriter, data projectsView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.projectsTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProjects(w, r)
	case http.MethodPost:
		s.createProject(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	data := projectsView{
		basePage:   s.newBasePage("projects"),
		ShowHidden: r.URL.Query().Get("hidden") == "1",
	}

	store := s.getProjectStore()
	if store == nil {
		data.Error = "Project store not available"
		s.renderProjects(w, data)
		return
	}

	// Respect ?flash/?error from redirect-after-action.
	if f := r.URL.Query().Get("flash"); f != "" {
		data.Flash = f
	}
	if e := r.URL.Query().Get("error"); e != "" {
		data.Error = e
	}

	projects, err := store.List(data.ShowHidden)
	if err != nil {
		data.Error = fmt.Sprintf("list projects: %v", err)
		s.renderProjects(w, data)
		return
	}

	activeSlug := s.activeProjectSlug()

	dirs := s.countDirectories(projects)

	editSlug := strings.TrimSpace(r.URL.Query().Get("edit"))
	for _, p := range projects {
		row := projectRow{
			Slug:           p.Slug,
			DisplayName:    p.DisplayName,
			Hidden:         p.Hidden,
			IsGlobal:       p.Slug == project.GlobalSlug,
			IsActive:       p.Slug == activeSlug,
			DirectoryCount: dirs[p.Slug],
			MemoryRepoPath: p.MemoryRepoPath,
		}
		if p.ModelPath != nil {
			row.ModelPath = *p.ModelPath
		}
		if p.ModelBinary != nil {
			row.ModelBinary = *p.ModelBinary
		}
		if p.ModelCtxSize != nil {
			row.ModelCtxSize = strconv.Itoa(*p.ModelCtxSize)
		}
		if p.ModelGPULayers != nil {
			row.ModelGPULayers = strconv.Itoa(*p.ModelGPULayers)
		}
		if p.ModelNParallel != nil {
			row.ModelNParallel = strconv.Itoa(*p.ModelNParallel)
		}
		data.Projects = append(data.Projects, row)
		if editSlug != "" && editSlug == row.Slug && !row.IsGlobal {
			editRow := row
			data.EditProject = &editRow
		}
	}

	s.renderProjects(w, data)
}

func (s *Server) handleProjectActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("parse form: "+err.Error()), http.StatusSeeOther)
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("slug is required"), http.StatusSeeOther)
		return
	}
	if err := s.activateProject(slug); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.refreshProjectNav()
	http.Redirect(w, r, "/projects?flash="+url.QueryEscape(fmt.Sprintf("Activated project %q. The harness is reloading.", slug)), http.StatusSeeOther)
}

func (s *Server) handleProjectHide(w http.ResponseWriter, r *http.Request) {
	s.handleProjectHidden(w, r, true)
}

func (s *Server) handleProjectUnhide(w http.ResponseWriter, r *http.Request) {
	s.handleProjectHidden(w, r, false)
}

func (s *Server) handleProjectHidden(w http.ResponseWriter, r *http.Request, hidden bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	store := s.getProjectStore()
	if store == nil {
		http.Error(w, "project store not available", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("parse form: "+err.Error()), http.StatusSeeOther)
		return
	}
	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("slug is required"), http.StatusSeeOther)
		return
	}
	if err := store.SetHidden(slug, hidden); err != nil {
		action := "hide"
		if !hidden {
			action = "unhide"
		}
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(fmt.Sprintf("%s: %v", action, err)), http.StatusSeeOther)
		return
	}
	s.refreshProjectNav()
	message := fmt.Sprintf("Project %q hidden.", slug)
	if !hidden {
		message = fmt.Sprintf("Project %q unhidden.", slug)
	}
	http.Redirect(w, r, "/projects?flash="+url.QueryEscape(message), http.StatusSeeOther)
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	store := s.getProjectStore()
	if store == nil {
		http.Error(w, "project store not available", http.StatusServiceUnavailable)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("parse form: "+err.Error()), http.StatusSeeOther)
		return
	}

	displayName := strings.TrimSpace(r.FormValue("display_name"))
	slug := strings.TrimSpace(r.FormValue("slug"))

	if slug == "" {
		slug = slugFromName(displayName)
	}

	if err := project.ValidateCreatableSlug(slug); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := project.ValidateDisplayName(displayName); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	dirs := parseDirectories(r.FormValue("directories"))

	input := project.CreateInput{
		Slug:           slug,
		DisplayName:    displayName,
		ModelBinary:    optionalString(r.FormValue("model_binary")),
		ModelPath:      optionalString(r.FormValue("model_path")),
		ModelCtxSize:   optionalInt(r.FormValue("model_ctx_size")),
		ModelGPULayers: optionalInt(r.FormValue("model_gpu_layers")),
		ModelNParallel: optionalInt(r.FormValue("model_n_parallel")),
		Directories:    dirs,
		MemoryRepoPath: trimPathField(r.FormValue("memory_repo_path")),
	}

	workflow := project.NewWorkflow(store, memory.ProjectRepoManager{})
	if _, err := workflow.Create(input); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.refreshProjectNav()

	http.Redirect(w, r, "/projects?flash="+url.QueryEscape("Project "+slug+" created."), http.StatusSeeOther)
}

func (s *Server) handleProjectEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("parse form: "+err.Error()), http.StatusSeeOther)
		return
	}

	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("slug is required"), http.StatusSeeOther)
		return
	}

	input := project.UpdateInput{
		Slug:           slug,
		DisplayName:    strings.TrimSpace(r.FormValue("display_name")),
		MemoryRepoPath: trimPathField(r.FormValue("memory_repo_path")),
		ModelBinary:    optionalString(r.FormValue("model_binary")),
		ModelPath:      optionalString(r.FormValue("model_path")),
		ModelCtxSize:   optionalInt(r.FormValue("model_ctx_size")),
		ModelGPULayers: optionalInt(r.FormValue("model_gpu_layers")),
		ModelNParallel: optionalInt(r.FormValue("model_n_parallel")),
	}

	if err := project.ValidateDisplayName(input.DisplayName); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if input.MemoryRepoPath == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("memory repo path is required"), http.StatusSeeOther)
		return
	}

	// Edits route through the runtime-owned project editor, which serializes
	// the edit with the apply transaction, refuses to move the active
	// project's memory repository, and re-applies the live system for
	// active-project edits. The handler never constructs project.Workflow
	// directly, so a store mutation can never silently diverge from the live
	// generation.
	edit := s.getProjectEditor()
	if edit == nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("project editor not available"), http.StatusSeeOther)
		return
	}
	if _, err := edit(input, r.FormValue("memory_repo_mode")); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.refreshProjectNav()

	http.Redirect(w, r, "/projects?flash="+url.QueryEscape("Project "+slug+" updated."), http.StatusSeeOther)
}

func sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// activateProject saves the active project slug and triggers a config
// re-apply via the retry callback, which handles llama-server reload
// per llama_on_switch.
func (s *Server) activateProject(slug string) error {
	store := s.configStore()
	if store == nil {
		return fmt.Errorf("config store not available")
	}
	targeted, ok := store.(interface{ SetActiveProjectSlug(string) error })
	if !ok {
		return fmt.Errorf("config store does not support targeted project updates")
	}
	if err := targeted.SetActiveProjectSlug(slug); err != nil {
		return fmt.Errorf("set active project: %w", err)
	}
	s.state.mu.Lock()
	s.state.data.ProjectSlug = slug
	s.state.mu.Unlock()
	slog.Info("project activated", "slug", slug)
	s.callRetry()
	return nil
}

// activeProjectSlug returns the currently active project slug.
func (s *Server) activeProjectSlug() string {
	store := s.configStore()
	if store == nil {
		return s.activeProjectSlugFallback()
	}
	loaded, _, err := store.Load()
	if err != nil {
		return s.activeProjectSlugFallback()
	}
	if loaded.Project.ActiveProjectSlug == "" {
		return s.activeProjectSlugFallback()
	}
	return loaded.Project.ActiveProjectSlug
}

func (s *Server) activeProjectSlugFallback() string {
	snap := s.state.snapshot()
	if snap.ProjectSlug != "" {
		return snap.ProjectSlug
	}
	return project.GlobalSlug
}

// countDirectories returns per-project directory counts.
func (s *Server) countDirectories(projects []project.Project) map[string]int {
	store := s.getProjectStore()
	if store == nil {
		return map[string]int{}
	}
	out := map[string]int{}
	for _, p := range projects {
		dirs, err := store.ListDirectories(p.Slug)
		if err != nil {
			continue
		}
		out[p.Slug] = len(dirs)
	}
	return out
}

// slugFromName derives a lowercase-dashed slug from a display name.
func slugFromName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	cleaned := strings.Builder{}
	lastWasAlnum := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			if r == '-' {
				if !lastWasAlnum || cleaned.Len() == 0 {
					continue
				}
				lastWasAlnum = false
			} else {
				lastWasAlnum = true
			}
			cleaned.WriteRune(r)
		}
	}
	return strings.Trim(cleaned.String(), "-")
}

func optionalString(v string) *string {
	v = trimPathField(v)
	if v == "" {
		return nil
	}
	return &v
}

func optionalInt(v string) *int {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

func parseDirectories(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(raw, "\n") {
		line = trimPathField(line)
		if line == "" {
			continue
		}
		dirs = append(dirs, line)
	}
	return dirs
}
