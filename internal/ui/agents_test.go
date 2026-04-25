package ui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// stubRegistry is a test double that records the last SetActive/Create calls
// and can be primed to return a scripted error from each method.
type stubRegistry struct {
	mu          sync.Mutex
	agents      []AgentInfo
	active      atomic.Value // string
	setCalls    atomic.Int32
	lastSet     atomic.Value // string
	createCalls atomic.Int32
	lastCreate  atomic.Value // string

	listErr   error
	getErr    error
	setErr    error
	createErr error
}

func newStubRegistry(active string, agents ...AgentInfo) *stubRegistry {
	r := &stubRegistry{agents: agents}
	r.active.Store(active)
	r.lastSet.Store("")
	r.lastCreate.Store("")
	return r
}

func (r *stubRegistry) List() ([]AgentInfo, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	r.mu.Lock()
	out := make([]AgentInfo, len(r.agents))
	copy(out, r.agents)
	r.mu.Unlock()
	return out, nil
}

func (r *stubRegistry) Get(name string) (AgentInfo, error) {
	if r.getErr != nil {
		return AgentInfo{}, r.getErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.agents {
		if a.Name == name {
			return a, nil
		}
	}
	return AgentInfo{}, errors.New("agent not found: " + name)
}

func (r *stubRegistry) Active() string {
	v, _ := r.active.Load().(string)
	return v
}

func (r *stubRegistry) SetActive(name string) error {
	r.setCalls.Add(1)
	r.lastSet.Store(name)
	if r.setErr != nil {
		return r.setErr
	}
	r.active.Store(name)
	return nil
}

func (r *stubRegistry) Create(name string) error {
	r.createCalls.Add(1)
	r.lastCreate.Store(name)
	if r.createErr != nil {
		return r.createErr
	}
	r.mu.Lock()
	r.agents = append(r.agents, AgentInfo{Name: name, PersonaPath: "agents/" + name + "/persona.md"})
	r.mu.Unlock()
	return nil
}

func (r *stubRegistry) lastSetActive() string {
	v, _ := r.lastSet.Load().(string)
	return v
}

func (r *stubRegistry) lastCreated() string {
	v, _ := r.lastCreate.Load().(string)
	return v
}

func TestHandleAgents_GETRendersList(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("coder",
		AgentInfo{Name: "coder", PersonaPath: "agents/coder/persona.md", Persona: "You are a coder."},
		AgentInfo{Name: "reviewer", PersonaPath: "agents/reviewer/persona.md", Persona: "You review code."},
	)
	s.SetAgentRegistry(reg)

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"coder", "reviewer", "You are a coder."} {
		if !strings.Contains(body, want) {
			t.Errorf("agents body missing %q", want)
		}
	}
}

func TestHandleAgents_GETWithoutRegistryShowsSetupCTA(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Memory repo not configured") {
		t.Errorf("expected not-configured message, got:\n%s", body)
	}
}

func TestHandleAgents_GETEmptyRegistryShowsCreateForm(t *testing.T) {
	s := NewServer(3000)
	s.SetAgentRegistry(newStubRegistry(""))

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"No agents yet", `action="/agents/create"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected empty-state body to contain %q, got:\n%s", want, body)
		}
	}
}

func TestHandleAgents_GETSurfacesListError(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("")
	reg.listErr = errors.New("memory repo busy")
	s.SetAgentRegistry(reg)

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "memory repo busy") {
		t.Errorf("expected list error surfaced in body, got:\n%s", rec.Body.String())
	}
}

func TestHandleAgents_POSTMethodNotAllowedOnGET(t *testing.T) {
	s := NewServer(3000)
	s.SetAgentRegistry(newStubRegistry(""))

	req := httptest.NewRequest(http.MethodGet, "/agents/active", nil)
	rec := httptest.NewRecorder()
	s.handleAgentsActive(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestHandleAgents_POSTSwitchesActive(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("coder",
		AgentInfo{Name: "coder", Persona: "c"},
		AgentInfo{Name: "reviewer", Persona: "r"},
	)
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "reviewer")
	req := httptest.NewRequest(http.MethodPost, "/agents/active", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsActive(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/agents" {
		t.Errorf("expected redirect to /agents, got %q", got)
	}
	if got := reg.setCalls.Load(); got != 1 {
		t.Errorf("expected SetActive called once, got %d", got)
	}
	if got := reg.lastSetActive(); got != "reviewer" {
		t.Errorf("expected SetActive(\"reviewer\"), got %q", got)
	}
}

func TestHandleAgents_POSTEmptyNameClearsActive(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("coder", AgentInfo{Name: "coder"})
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "")
	req := httptest.NewRequest(http.MethodPost, "/agents/active", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsActive(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/agents" {
		t.Errorf("expected redirect to /agents, got %q", got)
	}
	if got := reg.setCalls.Load(); got != 1 {
		t.Errorf("expected SetActive called once, got %d", got)
	}
	if got := reg.lastSetActive(); got != "" {
		t.Errorf("expected SetActive(\"\"), got %q", got)
	}
}

func TestHandleAgents_POSTUnknownNameReturns400(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("", AgentInfo{Name: "coder"})
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "ghost")
	req := httptest.NewRequest(http.MethodPost, "/agents/active", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsActive(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown agent, got %d", rec.Code)
	}
	if got := reg.setCalls.Load(); got != 0 {
		t.Errorf("expected SetActive not called for unknown agent, got %d calls", got)
	}
}

func TestHandleAgents_POSTNoRegistryReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodPost, "/agents/active", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.handleAgentsActive(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when registry is nil, got %d", rec.Code)
	}
}

func TestHandleAgents_MethodNotAllowedOnPOST(t *testing.T) {
	s := NewServer(3000)
	s.SetAgentRegistry(newStubRegistry(""))

	req := httptest.NewRequest(http.MethodPost, "/agents", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST /agents, got %d", rec.Code)
	}
}

func TestHandleAgentsCreate_POSTRedirectsOnSuccess(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("")
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "coder")
	req := httptest.NewRequest(http.MethodPost, "/agents/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), "/agents?created=coder"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := reg.createCalls.Load(); got != 1 {
		t.Errorf("expected Create called once, got %d", got)
	}
	if got := reg.lastCreated(); got != "coder" {
		t.Errorf("expected Create(\"coder\"), got %q", got)
	}
}

func TestHandleAgentsCreate_POSTTrimsName(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("")
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "  coder  ")
	req := httptest.NewRequest(http.MethodPost, "/agents/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsCreate(rec, req)

	if got := reg.lastCreated(); got != "coder" {
		t.Errorf("expected trimmed Create(\"coder\"), got %q", got)
	}
}

func TestHandleAgentsCreate_POSTValidationErrorRendersForm(t *testing.T) {
	s := NewServer(3000)
	reg := newStubRegistry("", AgentInfo{Name: "coder", Persona: "c"})
	reg.createErr = errors.New("agent: invalid name: name is empty")
	s.SetAgentRegistry(reg)

	form := url.Values{}
	form.Set("name", "bad name")
	req := httptest.NewRequest(http.MethodPost, "/agents/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsCreate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"agent: invalid name", "bad name", "coder"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q, got:\n%s", want, body)
		}
	}
}

func TestHandleAgentsCreate_POSTNoRegistryReturns503(t *testing.T) {
	s := NewServer(3000)

	req := httptest.NewRequest(http.MethodPost, "/agents/create", strings.NewReader("name=coder"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleAgentsCreate(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when registry is nil, got %d", rec.Code)
	}
}

func TestHandleAgentsCreate_GETMethodNotAllowed(t *testing.T) {
	s := NewServer(3000)
	s.SetAgentRegistry(newStubRegistry(""))

	req := httptest.NewRequest(http.MethodGet, "/agents/create", nil)
	rec := httptest.NewRecorder()
	s.handleAgentsCreate(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for GET /agents/create, got %d", rec.Code)
	}
}

func TestHandleAgents_GETShowsCreatedFlash(t *testing.T) {
	s := NewServer(3000)
	s.SetAgentRegistry(newStubRegistry("", AgentInfo{Name: "coder"}))

	req := httptest.NewRequest(http.MethodGet, "/agents?created=coder", nil)
	rec := httptest.NewRecorder()
	s.handleAgents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Created agent") {
		t.Errorf("expected created flash, got:\n%s", body)
	}
}
