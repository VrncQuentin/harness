package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitw "github.com/vrnc/harness/internal/git"
	"github.com/vrnc/harness/internal/memory"
	"github.com/vrnc/harness/internal/project"
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
		MemoryRepoPath: strings.TrimSpace(r.FormValue("memory_repo_path")),
	}

	created, err := store.Create(input)
	if err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if err := prepareProjectMemoryRepo(created.MemoryRepoPath, created.Slug == project.GlobalSlug); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("project created, but memory repo setup failed: "+err.Error()), http.StatusSeeOther)
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
		MemoryRepoPath: strings.TrimSpace(r.FormValue("memory_repo_path")),
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

	current, err := store.Get(slug)
	if err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if input.MemoryRepoPath != "" && current.MemoryRepoPath != "" && !samePath(input.MemoryRepoPath, current.MemoryRepoPath) {
		switch r.FormValue("memory_repo_mode") {
		case "move":
			if err := copyProjectMemoryRepo(current.MemoryRepoPath, input.MemoryRepoPath, slug == project.GlobalSlug); err != nil {
				http.Redirect(w, r, "/projects?error="+url.QueryEscape("move memory repo: "+err.Error()), http.StatusSeeOther)
				return
			}
		case "fresh":
			if err := prepareProjectMemoryRepo(input.MemoryRepoPath, slug == project.GlobalSlug); err != nil {
				http.Redirect(w, r, "/projects?error="+url.QueryEscape("initialize memory repo: "+err.Error()), http.StatusSeeOther)
				return
			}
		default:
			http.Redirect(w, r, "/projects?error="+url.QueryEscape("choose whether to move existing memory data or start fresh"), http.StatusSeeOther)
			return
		}
	}

	if _, err := store.Update(input); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/projects?flash="+url.QueryEscape("Project "+slug+" updated."), http.StatusSeeOther)
}

func (s *Server) handleProjectBackup(w http.ResponseWriter, r *http.Request) {
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
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("slug is required"), http.StatusSeeOther)
		return
	}
	proj, err := store.Get(slug)
	if err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if _, err := exec.LookPath("gh"); err != nil {
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("GitHub backup requires the GitHub CLI (gh) to be installed and logged in."), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	flash := "GitHub backup created for " + slug + "."
	if projectMemoryRepoHasOrigin(proj.MemoryRepoPath) {
		if _, err := exec.LookPath("git"); err != nil {
			http.Redirect(w, r, "/projects?error="+url.QueryEscape("GitHub backup found an origin remote, but git was not available to push it."), http.StatusSeeOther)
			return
		}
		cmd = exec.CommandContext(ctx, "git", "-C", proj.MemoryRepoPath, "push", "-u", "origin", "HEAD")
		flash = "GitHub backup pushed for " + slug + "."
	} else {
		repoName := "harness-memory-" + slug
		cmd = exec.CommandContext(ctx, "gh", "repo", "create", repoName, "--private", "--source", proj.MemoryRepoPath, "--remote", "origin", "--push")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		http.Redirect(w, r, "/projects?error="+url.QueryEscape("GitHub backup failed: "+msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/projects?flash="+url.QueryEscape(flash), http.StatusSeeOther)
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

func projectMemoryRepoHasOrigin(repoPath string) bool {
	body, err := os.ReadFile(filepath.Join(repoPath, ".git", "config"))
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "[remote \"origin\"]")
}

func prepareProjectMemoryRepo(repoPath string, global bool) error {
	repo, err := gitw.Init(repoPath)
	if err != nil {
		return err
	}
	if err := memory.CreateMissingProjectRepo(repoPath, global); err != nil {
		return err
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "scaffold"}, "initialize project memory repo"), memory.ProjectRepoScaffoldFiles(global)); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
		slog.Warn("project memory repo scaffold commit", "repo", repoPath, "err", err)
	}
	return nil
}

func copyProjectMemoryRepo(src, dst string, global bool) error {
	if samePath(src, dst) {
		return prepareProjectMemoryRepo(dst, global)
	}
	if err := copyTreeWithoutGit(src, dst); err != nil {
		return err
	}
	if err := prepareProjectMemoryRepo(dst, global); err != nil {
		return err
	}
	repo, err := gitw.Open(dst)
	if err != nil {
		return err
	}
	files, err := listRepoFiles(dst)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	if _, err := repo.Commit(gitw.BuildMessage(map[string]string{"type": "migration"}, "move project memory repo"), files); err != nil && !errors.Is(err, gogit.ErrEmptyCommit) {
		slog.Warn("project memory repo move commit", "repo", dst, "err", err)
	}
	return nil
}

func copyTreeWithoutGit(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory: %s", src)
	}
	return filepath.WalkDir(src, func(srcPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(src, srcPath)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dstPath := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}
		return copyFile(srcPath, dstPath)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

func listRepoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

func samePath(a, b string) bool {
	ac, aerr := filepath.Abs(filepath.Clean(a))
	bc, berr := filepath.Abs(filepath.Clean(b))
	if aerr != nil || berr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.EqualFold(ac, bc)
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
