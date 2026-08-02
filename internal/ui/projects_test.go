package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/project"
)

// TestHandleProjectEditRoutesThroughEditor verifies that /projects/edit routes
// through the Runtime-owned project editor instead of mutating the project
// store directly. The discriminator: if the handler bypassed the editor (the
// pre-PR-10 behavior), the installed editor closure would never fire and the
// test would fail on a nil invocation.
func TestHandleProjectEditRoutesThroughEditor(t *testing.T) {
	s := NewServer(3000)

	var gotInput project.UpdateInput
	var gotMode string
	s.SetProjectEditor(func(input project.UpdateInput, mode string) (project.Project, error) {
		gotInput = input
		gotMode = mode
		return project.Project{Slug: input.Slug, DisplayName: input.DisplayName, MemoryRepoPath: input.MemoryRepoPath}, nil
	})

	form := url.Values{
		"display_name":     {"Demo Renamed"},
		"memory_repo_path": {"C:\\repo\\demo"},
		"memory_repo_mode": {"move"},
		"model_binary":     {"project-llama"},
		"model_path":       {"project.gguf"},
		"model_ctx_size":   {"4096"},
		"model_gpu_layers": {"20"},
		"model_n_parallel": {"2"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/edit?slug=demo", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleProjectEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "Project+demo+updated") {
		t.Fatalf("redirect location = %q, want success flash", loc)
	}

	if gotInput.Slug != "demo" {
		t.Fatalf("editor input slug = %q, want demo", gotInput.Slug)
	}
	if gotInput.DisplayName != "Demo Renamed" || gotInput.MemoryRepoPath != "C:\\repo\\demo" {
		t.Fatalf("editor input = %+v, want parsed display name and repo path", gotInput)
	}
	if gotMode != "move" {
		t.Fatalf("editor mode = %q, want move", gotMode)
	}
	if gotInput.ModelBinary == nil || *gotInput.ModelBinary != "project-llama" {
		t.Fatalf("editor model binary not parsed: %+v", gotInput.ModelBinary)
	}
	if gotInput.ModelCtxSize == nil || *gotInput.ModelCtxSize != 4096 {
		t.Fatalf("editor model ctx size not parsed: %+v", gotInput.ModelCtxSize)
	}
}

// TestHandleProjectEditWithoutEditorRedirectsWithError verifies that a project
// edit with no runtime editor installed fails loudly with a redirect rather
// than silently mutating the project store or serving a 500.
func TestHandleProjectEditWithoutEditorRedirectsWithError(t *testing.T) {
	s := NewServer(3000)

	form := url.Values{
		"display_name":     {"Demo"},
		"memory_repo_path": {"C:\\repo\\demo"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/edit?slug=demo", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleProjectEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect with an error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "project+editor+not+available") {
		t.Fatalf("redirect = %q, want editor-not-available error", rec.Header().Get("Location"))
	}
}

// TestHandleProjectEditSurfacesEditorError verifies that an editor rejection
// (e.g. ErrActiveProjectRepoMove) is surfaced to the user as a redirect error
// rather than a bare 500.
func TestHandleProjectEditSurfacesEditorError(t *testing.T) {
	s := NewServer(3000)
	s.SetProjectEditor(func(project.UpdateInput, string) (project.Project, error) {
		return project.Project{}, errors.New("active project memory repo cannot be moved while it is in use")
	})

	form := url.Values{
		"display_name":     {"Demo"},
		"memory_repo_path": {"C:\\repo\\other"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/edit?slug=demo", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleProjectEdit(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect with the editor error, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Location"), "cannot+be+moved+while+it+is+in+use") {
		t.Fatalf("redirect = %q, want the editor's refusal surfaced", rec.Header().Get("Location"))
	}
}
