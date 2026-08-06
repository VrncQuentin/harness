package config

import (
	"strings"
	"testing"

	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/tools"
)

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.UI.Port != 3000 {
		t.Errorf("expected default UI port 3000, got %d", d.UI.Port)
	}
	if d.Queue.MaxDepth != 8 {
		t.Errorf("expected default queue depth 8, got %d", d.Queue.MaxDepth)
	}
	if d.Metrics.RetentionDays != 30 {
		t.Errorf("expected default retention 30, got %d", d.Metrics.RetentionDays)
	}
	if !d.UI.OpenOnStart {
		t.Error("expected default UI.OpenOnStart=true")
	}
	if d.Log.RingMaxEntries != 500 {
		t.Errorf("expected default Log.RingMaxEntries 500, got %d", d.Log.RingMaxEntries)
	}
	if d.Log.ProcMaxLines != 64 {
		t.Errorf("expected default Log.ProcMaxLines 64, got %d", d.Log.ProcMaxLines)
	}
	if d.Endpoints.List[0].CacheTypeK != "q8_0" {
		t.Errorf("expected default local endpoint CacheTypeK q8_0, got %q", d.Endpoints.List[0].CacheTypeK)
	}
	if d.Endpoints.List[0].CacheTypeV != "q8_0" {
		t.Errorf("expected default local endpoint CacheTypeV q8_0, got %q", d.Endpoints.List[0].CacheTypeV)
	}
	if d.Endpoints.Active != "local" {
		t.Errorf("expected default active endpoint local, got %q", d.Endpoints.Active)
	}
	if len(d.Endpoints.List) != 1 {
		t.Errorf("expected one default endpoint, got %d", len(d.Endpoints.List))
	}
	if d.Project.ActiveProjectSlug != "global" {
		t.Errorf("expected default Project.ActiveProjectSlug global, got %q", d.Project.ActiveProjectSlug)
	}
	if d.Project.LlamaOnSwitch != "reload" {
		t.Errorf("expected default Project.LlamaOnSwitch reload, got %q", d.Project.LlamaOnSwitch)
	}
	if d.UI.SidebarRecentSessions != 5 {
		t.Errorf("expected default UI.SidebarRecentSessions 5, got %d", d.UI.SidebarRecentSessions)
	}
	for _, desc := range tools.BuiltinDescriptors() {
		if got := d.Loop.ToolEnabled(desc.ID); got != desc.DefaultEnabled {
			t.Errorf("default Loop.ToolEnabled(%q) = %v, want descriptor default %v", desc.ID, got, desc.DefaultEnabled)
		}
	}
}

// validCfg returns a Config that passes Validate, as a starting point for
// table entries to mutate a single field into an error state.
func validCfg() Config {
	c := Defaults()
	c.Endpoints.List[0].Binary = "/llama-server"
	c.Endpoints.List[0].ModelPath = "/models/model.gguf"
	c.Embedder.Binary = "/embedder"
	c.Embedder.ModelPath = "/models/nomic.gguf"
	return c
}

// externalCfg returns a Config whose active endpoint is a valid external
// OpenAI-compatible backend, as a starting point for external-endpoint cases.
func externalCfg() Config {
	c := validCfg()
	c.Endpoints = EndpointsConfig{
		Active:      "remote",
		ActiveModel: "llama3.2",
		List: []Endpoint{{
			ID:      "remote",
			Kind:    EndpointKindOpenAI,
			Name:    "Remote",
			BaseURL: "https://api.example.com/v1",
			APIKey:  "sk-test",
			Models: []EndpointModel{
				{ID: "llama3.2", Name: "Llama 3.2", CtxSize: 32768},
				{ID: "qwen", Name: "Qwen", CtxSize: 32768},
			},
		}},
	}
	return c
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // substring; empty means Validate must succeed
	}{
		{
			name:   "valid defaults with required paths",
			mutate: func(*Config) {},
		},

		// Required strings.
		{
			name:    "missing model binary",
			mutate:  func(c *Config) { c.Endpoints.List[0].Binary = "" },
			wantErr: "model.binary is required",
		},
		{
			name:    "whitespace-only model binary",
			mutate:  func(c *Config) { c.Endpoints.List[0].Binary = "   " },
			wantErr: "model.binary is required",
		},
		{
			name:    "missing model path",
			mutate:  func(c *Config) { c.Endpoints.List[0].ModelPath = "" },
			wantErr: "model.model_path is required",
		},
		{
			name:    "missing embedder binary",
			mutate:  func(c *Config) { c.Embedder.Binary = "" },
			wantErr: "embedder.binary is required",
		},
		{
			name:    "missing embedder model path",
			mutate:  func(c *Config) { c.Embedder.ModelPath = "" },
			wantErr: "embedder.model_path is required",
		},

		// Endpoint list shape.
		{
			name:    "no endpoints",
			mutate:  func(c *Config) { c.Endpoints.List = nil },
			wantErr: "at least one model endpoint is required",
		},
		{
			name:    "endpoint id empty",
			mutate:  func(c *Config) { c.Endpoints.List[0].ID = "" },
			wantErr: "endpoint id is required",
		},
		{
			name: "duplicate endpoint id",
			mutate: func(c *Config) {
				c.Endpoints.List = append(c.Endpoints.List, c.Endpoints.List[0])
			},
			wantErr: "duplicate endpoint id",
		},
		{
			name:    "invalid endpoint kind",
			mutate:  func(c *Config) { c.Endpoints.List[0].Kind = "grpc" },
			wantErr: "endpoint kind must be local or openai",
		},
		{
			name:    "unknown active endpoint",
			mutate:  func(c *Config) { c.Endpoints.Active = "nope" },
			wantErr: "active endpoint \"nope\" does not exist",
		},

		// Port ranges.
		{
			name:    "model port zero",
			mutate:  func(c *Config) { c.Endpoints.List[0].Port = 0 },
			wantErr: "model.port must be between 1 and 65535",
		},
		{
			name:    "model port negative",
			mutate:  func(c *Config) { c.Endpoints.List[0].Port = -1 },
			wantErr: "model.port must be between 1 and 65535",
		},
		{
			name:    "model port too high",
			mutate:  func(c *Config) { c.Endpoints.List[0].Port = 70000 },
			wantErr: "model.port must be between 1 and 65535",
		},
		{
			name:    "embedder port zero",
			mutate:  func(c *Config) { c.Embedder.Port = 0 },
			wantErr: "embedder.port must be between 1 and 65535",
		},
		{
			name:    "ui port zero",
			mutate:  func(c *Config) { c.UI.Port = 0 },
			wantErr: "ui.port must be between 1 and 65535",
		},
		{
			name:    "sidebar recent sessions out of range",
			mutate:  func(c *Config) { c.UI.SidebarRecentSessions = 11 },
			wantErr: "ui.sidebar_recent_sessions must be between 0 and 10",
		},
		{
			name:   "sidebar recent sessions zero disables",
			mutate: func(c *Config) { c.UI.SidebarRecentSessions = 0 },
		},
		{
			name:    "api port zero",
			mutate:  func(c *Config) { c.API.Port = 0 },
			wantErr: "api.port must be between 1 and 65535",
		},

		// Port collisions.
		{
			name:    "model and embedder collide",
			mutate:  func(c *Config) { c.Embedder.Port = c.Endpoints.List[0].Port },
			wantErr: "both use port",
		},
		{
			name:    "ui and api collide",
			mutate:  func(c *Config) { c.API.Port = c.UI.Port },
			wantErr: "both use port",
		},
		{
			name: "collision detected even when api disabled",
			mutate: func(c *Config) {
				c.API.Enabled = false
				c.API.Port = c.UI.Port
			},
			wantErr: "both use port",
		},
		{
			name: "two local endpoints collide",
			mutate: func(c *Config) {
				other := c.Endpoints.List[0]
				other.ID = "local2"
				c.Endpoints.List = append(c.Endpoints.List, other)
			},
			wantErr: "both use port",
		},

		// Model numeric bounds.
		{
			name:    "negative ctx size",
			mutate:  func(c *Config) { c.Endpoints.List[0].CtxSize = -1 },
			wantErr: "model.ctx_size must be >= 0",
		},
		{
			name:   "gpu layers -1 is allowed",
			mutate: func(c *Config) { c.Endpoints.List[0].GPULayers = -1 },
		},
		{
			name:    "gpu layers below -1",
			mutate:  func(c *Config) { c.Endpoints.List[0].GPULayers = -2 },
			wantErr: "model.gpu_layers must be >= -1",
		},
		{
			name:    "n_parallel zero",
			mutate:  func(c *Config) { c.Endpoints.List[0].NParallel = 0 },
			wantErr: "model.n_parallel must be >= 1",
		},

		// KV cache types.
		{
			name:    "empty cache_type_k",
			mutate:  func(c *Config) { c.Endpoints.List[0].CacheTypeK = "" },
			wantErr: "model.cache_type_k must be one of",
		},
		{
			name:    "unknown cache_type_v",
			mutate:  func(c *Config) { c.Endpoints.List[0].CacheTypeV = "q3_k" },
			wantErr: "model.cache_type_v must be one of",
		},
		{
			name:   "f16 cache types accepted",
			mutate: func(c *Config) { c.Endpoints.List[0].CacheTypeK = "f16"; c.Endpoints.List[0].CacheTypeV = "f16" },
		},

		// External endpoints.
		{
			name:   "valid external endpoint",
			mutate: func(c *Config) { c.Endpoints = externalCfg().Endpoints },
		},
		{
			name:    "external missing base url",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].BaseURL = "" },
			wantErr: "base_url is required",
		},
		{
			name:    "external invalid base url scheme",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].BaseURL = "ftp://nope" },
			wantErr: "base_url must be an http(s) URL",
		},
		{
			name:    "external invalid base url garbage",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].BaseURL = "not a url" },
			wantErr: "base_url must be an http(s) URL",
		},
		{
			name:    "external no models",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].Models = nil },
			wantErr: "at least one model",
		},
		{
			name:    "external empty model id",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].Models[0].ID = "" },
			wantErr: "model id is required",
		},
		{
			name:    "external duplicate model id",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].Models[1].ID = "llama3.2" },
			wantErr: "duplicate model id",
		},
		{
			name:    "external negative model ctx",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.List[0].Models[0].CtxSize = -5 },
			wantErr: "ctx_size must be >= 0",
		},
		{
			name:    "external active model unknown",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints; c.Endpoints.ActiveModel = "ghost" },
			wantErr: "active model \"ghost\" does not exist",
		},
		{
			name:    "external base url with api key",
			mutate:  func(c *Config) { c.Endpoints = externalCfg().Endpoints },
			wantErr: "",
		},

		// Prompt bounds + budget sum.
		{
			name:    "negative memory budget",
			mutate:  func(c *Config) { c.Prompt.MemoryTokenBudget = -1 },
			wantErr: "prompt.memory_token_budget must be >= 0",
		},
		{
			name:    "negative conversation reserve",
			mutate:  func(c *Config) { c.Prompt.ConversationReserve = -1 },
			wantErr: "prompt.conversation_reserve must be >= 0",
		},
		{
			name: "budget plus reserve exceeds ctx",
			mutate: func(c *Config) {
				c.Endpoints.List[0].CtxSize = 4096
				c.Prompt.MemoryTokenBudget = 3000
				c.Prompt.ConversationReserve = 2000
			},
			wantErr: "exceed model.ctx_size",
		},
		{
			name: "budget plus reserve equals ctx is fine",
			mutate: func(c *Config) {
				c.Endpoints.List[0].CtxSize = 8192
				c.Prompt.MemoryTokenBudget = 4096
				c.Prompt.ConversationReserve = 4096
			},
		},
		{
			name: "model ctx size zero skips budget check",
			mutate: func(c *Config) {
				c.Endpoints.List[0].CtxSize = 0
				c.Prompt.MemoryTokenBudget = 1000
				c.Prompt.ConversationReserve = 1000
			},
		},
		{
			name: "external model ctx bounds the budget",
			mutate: func(c *Config) {
				c.Endpoints = externalCfg().Endpoints
				c.Endpoints.List[0].Models[0].CtxSize = 2048
				c.Prompt.MemoryTokenBudget = 3000
				c.Prompt.ConversationReserve = 0
			},
			wantErr: "exceed model.ctx_size",
		},

		// Queue + metrics.
		{
			name:    "queue depth zero",
			mutate:  func(c *Config) { c.Queue.MaxDepth = 0 },
			wantErr: "queue.max_depth must be >= 1",
		},
		{
			name:    "retention zero",
			mutate:  func(c *Config) { c.Metrics.RetentionDays = 0 },
			wantErr: "metrics.retention_days must be >= 1",
		},

		// Log buffer bounds.
		{
			name:    "ring max entries zero",
			mutate:  func(c *Config) { c.Log.RingMaxEntries = 0 },
			wantErr: "log.ring_max_entries must be >= 1",
		},
		{
			name:    "ring max entries negative",
			mutate:  func(c *Config) { c.Log.RingMaxEntries = -10 },
			wantErr: "log.ring_max_entries must be >= 1",
		},
		{
			name:    "proc max lines zero",
			mutate:  func(c *Config) { c.Log.ProcMaxLines = 0 },
			wantErr: "log.proc_max_lines must be >= 1",
		},

		// Project fields.
		{
			name:    "empty active_project_slug",
			mutate:  func(c *Config) { c.Project.ActiveProjectSlug = "" },
			wantErr: "project.active_project_slug is required",
		},
		{
			name:    "whitespace-only active_project_slug",
			mutate:  func(c *Config) { c.Project.ActiveProjectSlug = "   " },
			wantErr: "project.active_project_slug is required",
		},
		{
			name:    "invalid active_project_slug",
			mutate:  func(c *Config) { c.Project.ActiveProjectSlug = "bad_slug" },
			wantErr: "project.active_project_slug: project: invalid slug",
		},
		{
			name:    "invalid llama_on_switch",
			mutate:  func(c *Config) { c.Project.LlamaOnSwitch = "restart" },
			wantErr: "project.llama_on_switch must be keep or reload",
		},
		{
			name:   "keep llama_on_switch accepted",
			mutate: func(c *Config) { c.Project.LlamaOnSwitch = "keep" },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg()
			tc.mutate(&cfg)
			err := Validate(&cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected Validate to pass, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestActiveModelConfig(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		c := validCfg()
		c.Endpoints.List[0].Binary = "/bin/llama-server"
		c.Endpoints.List[0].ModelPath = "/models/m.gguf"
		c.Endpoints.List[0].CtxSize = 16384
		c.Endpoints.List[0].Port = 9123
		m := c.ActiveModelConfig()
		if m.Kind != EndpointKindLocal {
			t.Fatalf("Kind = %q, want local", m.Kind)
		}
		if m.Binary != "/bin/llama-server" || m.ModelPath != "/models/m.gguf" {
			t.Errorf("local fields not projected: %+v", m)
		}
		if m.CtxSize != 16384 || m.Port != 9123 {
			t.Errorf("local ctx/port not projected: %+v", m)
		}
		if m.BaseURL != "" {
			t.Errorf("local endpoint must not project a BaseURL, got %q", m.BaseURL)
		}
	})

	t.Run("external picks active model", func(t *testing.T) {
		c := externalCfg()
		m := c.ActiveModelConfig()
		if m.Kind != EndpointKindOpenAI {
			t.Fatalf("Kind = %q, want openai", m.Kind)
		}
		if m.BaseURL != "https://api.example.com/v1" || m.APIKey != "sk-test" {
			t.Errorf("external url/key not projected: %+v", m)
		}
		if m.ModelID != "llama3.2" {
			t.Errorf("ModelID = %q, want llama3.2", m.ModelID)
		}
		if m.CtxSize != 32768 {
			t.Errorf("CtxSize = %d, want 32768", m.CtxSize)
		}
		if m.Binary != "" {
			t.Errorf("external endpoint must not project a Binary, got %q", m.Binary)
		}
	})

	t.Run("external falls back to first model", func(t *testing.T) {
		c := externalCfg()
		c.Endpoints.ActiveModel = ""
		m := c.ActiveModelConfig()
		if m.ModelID != "llama3.2" {
			t.Errorf("ModelID = %q, want first model llama3.2", m.ModelID)
		}
	})

	t.Run("external zero ctx uses default", func(t *testing.T) {
		c := externalCfg()
		c.Endpoints.List[0].Models[0].CtxSize = 0
		m := c.ActiveModelConfig()
		if m.CtxSize != defaultExternalCtxSize {
			t.Errorf("CtxSize = %d, want default %d", m.CtxSize, defaultExternalCtxSize)
		}
	})

	t.Run("unknown active endpoint yields zero value", func(t *testing.T) {
		c := validCfg()
		c.Endpoints.Active = "missing"
		m := c.ActiveModelConfig()
		if m.Kind != "" || m.Port != 0 {
			t.Errorf("expected zero ModelConfig, got %+v", m)
		}
	})
}

func TestEffectiveModelProjectOverridesOnlyApplyLocally(t *testing.T) {
	projBinary := "proj-llama"
	projCtx := 12345
	proj := &project.Project{}
	proj.ModelBinary = &projBinary
	proj.ModelCtxSize = &projCtx

	t.Run("local applies overrides", func(t *testing.T) {
		c := validCfg()
		m := EffectiveModel(&c, proj)
		if m.Binary != projBinary || m.CtxSize != projCtx {
			t.Errorf("project overrides not applied: %+v", m)
		}
		if m.Kind != EndpointKindLocal {
			t.Errorf("Kind = %q, want local", m.Kind)
		}
	})

	t.Run("external ignores overrides", func(t *testing.T) {
		c := externalCfg()
		m := EffectiveModel(&c, proj)
		if m.Binary != "" || m.ModelPath != "" {
			t.Errorf("external endpoint must ignore local overrides: %+v", m)
		}
		if m.BaseURL == "" || m.ModelID == "" {
			t.Errorf("external fields lost: %+v", m)
		}
	})
}

func TestModelConfigEqualKindAware(t *testing.T) {
	baseCfg := validCfg()
	base := baseCfg.ActiveModelConfig()
	// Local: Port and Verbose excluded.
	a := base
	a.Port = 8081
	b := base
	b.Port = 9999
	if !ModelConfigEqual(a, b) {
		t.Error("local configs differing only in Port should be equal")
	}
	a.Verbose = true
	b.Verbose = false
	if !ModelConfigEqual(a, b) {
		t.Error("local configs differing only in Verbose should be equal")
	}
	b.ModelPath = "/other.gguf"
	if ModelConfigEqual(a, b) {
		t.Error("local configs differing in ModelPath should not be equal")
	}

	// External: BaseURL / ModelID / CtxSize / APIKey matter.
	extCfg := externalCfg()
	ea := extCfg.ActiveModelConfig()
	eb := ea
	if !ModelConfigEqual(ea, eb) {
		t.Error("identical external configs should be equal")
	}
	eb.ModelID = "qwen"
	if ModelConfigEqual(ea, eb) {
		t.Error("external configs differing in ModelID should not be equal")
	}
	eb = ea
	eb.APIKey = "other"
	if ModelConfigEqual(ea, eb) {
		t.Error("external configs differing in APIKey should not be equal")
	}
	eb = ea
	eb.Kind = EndpointKindLocal
	if ModelConfigEqual(ea, eb) {
		t.Error("configs of different kinds should not be equal")
	}
}
