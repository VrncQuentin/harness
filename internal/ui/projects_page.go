package ui

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vrnc/harness/internal/project"
)

type projectRow struct {
	Slug           string
	DisplayName    string
	Hidden         bool
	IsGlobal       bool
	IsActive       bool
	DirectoryCount int
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

	// Handle actions from query params before rendering list.
	slug := r.URL.Query().Get("activate")
	if slug != "" {
		if err := s.activateProject(slug); err != nil {
			data.Error = err.Error()
		} else {
			data.Flash = fmt.Sprintf("Activated project %q. The harness is reloading.", slug)
		}
	}
	if slug := r.URL.Query().Get("hide"); slug != "" {
		if err := store.SetHidden(slug, true); err != nil {
			data.Error = fmt.Sprintf("hide: %v", err)
		} else {
			data.Flash = fmt.Sprintf("Project %q hidden.", slug)
			// Redirect to avoid re-trigger on refresh.
			http.Redirect(w, r, "/projects", http.StatusSeeOther)
			return
		}
	}
	if slug := r.URL.Query().Get("unhide"); slug != "" {
		if err := store.SetHidden(slug, false); err != nil {
			data.Error = fmt.Sprintf("unhide: %v", err)
		} else {
			data.Flash = fmt.Sprintf("Project %q unhidden.", slug)
			http.Redirect(w, r, "/projects", http.StatusSeeOther)
			return
		}
	}

	// Respect ?flash from redirect-after-create.
	if f := r.URL.Query().Get("flash"); f != "" {
		data.Flash = f
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
	}

	if _, err := store.Create(input); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/projects?flash="+url.QueryEscape("Project "+slug+" created."), http.StatusSeeOther)
}

func (s *Server) handleProjectEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("slug is required"), http.StatusSeeOther)
		return
	}

	input := project.UpdateInput{
		Slug:           slug,
		DisplayName:    strings.TrimSpace(r.FormValue("display_name")),
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

	if _, err := store.Update(input); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/projects?flash="+url.QueryEscape("Project "+slug+" updated."), http.StatusSeeOther)
}

// activateProject saves the active project slug and triggers a config
// re-apply via the retry callback, which handles llama-server reload
// per llama_on_switch.
func (s *Server) activateProject(slug string) error {
	store := s.configStore()
	if store == nil {
		return fmt.Errorf("config store not available")
	}
	loaded, _, err := store.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	loaded.Project.ActiveProjectSlug = slug
	if err := store.Save(loaded); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	slog.Info("project activated", "slug", slug)
	s.callRetry()
	return nil
}

// activeProjectSlug returns the currently active project slug.
func (s *Server) activeProjectSlug() string {
	store := s.configStore()
	if store == nil {
		return project.GlobalSlug
	}
	loaded, _, err := store.Load()
	if err != nil {
		return project.GlobalSlug
	}
	if loaded.Project.ActiveProjectSlug == "" {
		return project.GlobalSlug
	}
	return loaded.Project.ActiveProjectSlug
}

// countDirectories returns per-project directory counts.
func (s *Server) countDirectories(projects []project.Project) map[string]int {
	store := s.getProjectStore()
	if store == nil {
		return map[string]int{}
	}
	out := map[string]int{}
	for _, p := range projects {
		// The UI ProjectStore interface doesn't include ListDirectories.
		// Use a type assertion to access it, or fall back to 0.
		if fullStore, ok := store.(interface {
			ListDirectories(slug string) ([]project.Directory, error)
		}); ok {
			dirs, err := fullStore.ListDirectories(p.Slug)
			if err != nil {
				continue
			}
			out[p.Slug] = len(dirs)
		}
	}
	return out
}

// slugFromName derives a lowercase-dashed slug from a display name.
func slugFromName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	cleaned := strings.Builder{}
	invariant := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			if r == '-' {
				if !invariant || cleaned.Len() == 0 {
					continue
				}
				invariant = false
			} else {
				invariant = true
			}
			cleaned.WriteRune(r)
		}
	}
	return strings.Trim(cleaned.String(), "-")
}

func optionalString(v string) *string {
	v = strings.TrimSpace(v)
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
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dirs = append(dirs, line)
	}
	return dirs
}
