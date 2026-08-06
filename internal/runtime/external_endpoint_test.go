package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/inference"
	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/ui"
)

// externalEndpoints returns an endpoints config whose active endpoint is an
// external OpenAI-compatible backend with two models.
func externalEndpoints() config.EndpointsConfig {
	return config.EndpointsConfig{
		Active:      "remote",
		ActiveModel: "qwen",
		List: []config.Endpoint{{
			ID:      "remote",
			Kind:    config.EndpointKindOpenAI,
			Name:    "Remote",
			BaseURL: "https://api.example.com/v1",
			APIKey:  "sk-test",
			Models: []config.EndpointModel{
				{ID: "qwen", Name: "Qwen", CtxSize: 32768},
				{ID: "llama", Name: "Llama", CtxSize: 32768},
			},
		}},
	}
}

// TestNewInferenceClientForModelExternal sends a completion through a client
// built from an external model config and verifies the request reaches the
// endpoint's base URL with the selected model id and bearer API key.
func TestNewInferenceClientForModelExternal(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req inference.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = req.Model
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n")) //nolint:errcheck
	}))
	defer srv.Close()

	rt := New(config.Defaults(), nil, LogRings{})
	model := config.ModelConfig{
		Kind:       config.EndpointKindOpenAI,
		BaseURL:    srv.URL,
		APIKey:     "sk-secret",
		ModelID:    "qwen2.5",
		EndpointID: "remote",
	}
	client := rt.newInferenceClientForModel(model)
	ch, err := client.Complete(context.Background(), inference.CompletionRequest{
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for tok := range ch {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
	}
	if gotModel != "qwen2.5" {
		t.Errorf("Model = %q, want qwen2.5", gotModel)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want Bearer sk-secret", gotAuth)
	}
}

// TestNewInferenceClientForModelLocalTargetsPort verifies the local path keeps
// talking to the llama-server port.
func TestNewInferenceClientForModelLocalTargetsPort(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n")) //nolint:errcheck
	}))
	defer srv.Close()

	port := srv.Listener.Addr().(*net.TCPAddr).Port
	rt := New(config.Defaults(), nil, LogRings{})
	model := config.ModelConfig{Kind: config.EndpointKindLocal, Port: port}
	client := rt.newInferenceClientForModel(model)
	ch, err := client.Complete(context.Background(), inference.CompletionRequest{
		Messages: []inference.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for tok := range ch {
		if tok.Err != nil {
			t.Fatalf("token error: %v", tok.Err)
		}
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
}

// TestStartServicesSkipsLlamaForExternalEndpoint verifies that no llama-server
// manager is created when the active endpoint is external, while the embedder
// sidecar and the inference client still come up.
func TestStartServicesSkipsLlamaForExternalEndpoint(t *testing.T) {
	cfg := config.Defaults()
	cfg.Endpoints = externalEndpoints()
	cfg.Embedder.Binary = "embed-bin"
	cfg.Embedder.ModelPath = "embed.gguf"

	rt := New(cfg, nil, LogRings{})
	t.Cleanup(func() { stopRuntime(t, rt) })

	uiServer := ui.NewServer(0)
	rt.startServices(context.Background(), uiServer, NewEventChannel(), nil)

	if rt.llamaMgr != nil {
		t.Fatal("expected no llama-server manager for an external endpoint")
	}
	if rt.embedMgr == nil {
		t.Fatal("expected the embedder manager to be created regardless of the model backend")
	}
	if rt.inferClient == nil || rt.reqQueue == nil {
		t.Fatal("expected an inference client and queue")
	}
}

// TestApplyConfig_KindSwitchRequiresRestart verifies that switching between a
// local and an external backend is a restart-required change: the config is
// persisted (the caller saves it), the live applied state and process stay on
// the old backend, and the result reports "model backend". Even when the same
// save changes a live-readable field (metrics retention), the result must not
// claim a live apply it did not perform.
func TestApplyConfig_KindSwitchRequiresRestart(t *testing.T) {
	cfg := config.Defaults()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	if rt.applied == nil || rt.applied.runningModel.Kind != config.EndpointKindLocal {
		t.Fatal("precondition: applied running model must be local")
	}

	external := cfg
	external.Endpoints = externalEndpoints()
	external.Metrics.RetentionDays++
	rt.cfgStore = &runtimeConfigStore{cfg: &external, saved: true}

	// enterApply/leaveApply are paired seams; the kind-switch early return must
	// still run leaveApply like every other ApplyConfig exit.
	var applySeqs []string
	rt.enterApply = func() { applySeqs = append(applySeqs, "enter") }
	rt.leaveApply = func() { applySeqs = append(applySeqs, "leave") }

	uiServer := ui.NewServer(0)
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)

	if result.LiveApplied {
		t.Fatal("a backend kind switch must not live-apply, even for a metrics-retention change on the same save")
	}
	if !slices.Contains(result.RestartNeeded, "model backend") {
		t.Fatalf("RestartNeeded = %v, want it to include model backend", result.RestartNeeded)
	}
	if len(applySeqs) != 2 || applySeqs[0] != "enter" || applySeqs[1] != "leave" {
		t.Fatalf("apply seams = %v, want [enter leave]", applySeqs)
	}
	// The status UI must keep reflecting the live local backend, not the saved
	// external one (the restart-pending window).
	if uiServer.Backend().External {
		t.Fatalf("status UI must not report an external backend that is pending a restart: %+v", uiServer.Backend())
	}
	// The recorded applied state must still describe the live (local) backend.
	if rt.applied == nil || rt.applied.runningModel.Kind != config.EndpointKindLocal {
		t.Fatalf("applied running model must stay local until restart, got %+v", rt.applied)
	}
	if rt.llamaMgr == nil {
		t.Fatal("the local llama-server manager must keep running until restart")
	}
}

// TestApplyConfig_ExternalIdentityChangeAppliesLive verifies that changing the
// model id of the active external endpoint repoints the client live (no
// restart), rebuilding the generation so sessions use the new model.
func TestApplyConfig_ExternalIdentityChangeAppliesLive(t *testing.T) {
	cfg := config.Defaults()
	cfg.Endpoints = externalEndpoints()
	seedRequiredConfigFiles(t, &cfg)
	cfg.Project.ActiveProjectSlug = project.GlobalSlug

	rt, _ := appliedRuntimeForTest(t, &cfg, nil)
	if rt.applied == nil || rt.applied.runningModel.Kind != config.EndpointKindOpenAI {
		t.Fatal("precondition: applied running model must be external")
	}

	loaded := cfg
	loaded.Endpoints.ActiveModel = "llama"
	rt.cfgStore = &runtimeConfigStore{cfg: &loaded, saved: true}

	uiServer := ui.NewServer(0)
	result := rt.ApplyConfig(context.Background(), uiServer, NewEventChannel(), nil)
	if !result.LiveApplied {
		t.Fatalf("expected a live apply, got %+v", result)
	}
	if len(result.RestartNeeded) != 0 {
		t.Fatalf("RestartNeeded = %v, want none for an external model switch", result.RestartNeeded)
	}
	if rt.applied == nil || rt.applied.runningModel.ModelID != "llama" {
		t.Fatalf("applied running model = %+v, want ModelID llama", rt.applied)
	}
	b := uiServer.Backend()
	if !b.External || b.Endpoint != "remote" || b.Model != "llama" {
		t.Fatalf("status UI backend = %+v, want external endpoint remote model llama", b)
	}
}
