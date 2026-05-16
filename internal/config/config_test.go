package config

import (
	"strings"
	"testing"
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
	if d.Model.CacheTypeK != "q8_0" {
		t.Errorf("expected default Model.CacheTypeK q8_0, got %q", d.Model.CacheTypeK)
	}
	if d.Model.CacheTypeV != "q8_0" {
		t.Errorf("expected default Model.CacheTypeV q8_0, got %q", d.Model.CacheTypeV)
	}
	if d.Project.ActiveProjectSlug != "global" {
		t.Errorf("expected default Project.ActiveProjectSlug global, got %q", d.Project.ActiveProjectSlug)
	}
	if d.Project.LlamaOnSwitch != "reload" {
		t.Errorf("expected default Project.LlamaOnSwitch reload, got %q", d.Project.LlamaOnSwitch)
	}
}

// validCfg returns a Config that passes Validate, as a starting point for
// table entries to mutate a single field into an error state.
func validCfg() Config {
	c := Defaults()
	c.Model.Binary = "/llama-server"
	c.Model.ModelPath = "/models/model.gguf"
	c.Embedder.Binary = "/embedder"
	c.Embedder.ModelPath = "/models/nomic.gguf"
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
			mutate:  func(c *Config) { c.Model.Binary = "" },
			wantErr: "model.binary is required",
		},
		{
			name:    "whitespace-only model binary",
			mutate:  func(c *Config) { c.Model.Binary = "   " },
			wantErr: "model.binary is required",
		},
		{
			name:    "missing model path",
			mutate:  func(c *Config) { c.Model.ModelPath = "" },
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

		// Port ranges.
		{
			name:    "model port zero",
			mutate:  func(c *Config) { c.Model.Port = 0 },
			wantErr: "model.port must be between 1 and 65535",
		},
		{
			name:    "model port negative",
			mutate:  func(c *Config) { c.Model.Port = -1 },
			wantErr: "model.port must be between 1 and 65535",
		},
		{
			name:    "model port too high",
			mutate:  func(c *Config) { c.Model.Port = 70000 },
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
			name:    "api port zero",
			mutate:  func(c *Config) { c.API.Port = 0 },
			wantErr: "api.port must be between 1 and 65535",
		},

		// Port collisions.
		{
			name:    "model and embedder collide",
			mutate:  func(c *Config) { c.Embedder.Port = c.Model.Port },
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

		// Model numeric bounds.
		{
			name:    "negative ctx size",
			mutate:  func(c *Config) { c.Model.CtxSize = -1 },
			wantErr: "model.ctx_size must be >= 0",
		},
		{
			name:   "gpu layers -1 is allowed",
			mutate: func(c *Config) { c.Model.GPULayers = -1 },
		},
		{
			name:    "gpu layers below -1",
			mutate:  func(c *Config) { c.Model.GPULayers = -2 },
			wantErr: "model.gpu_layers must be >= -1",
		},
		{
			name:    "n_parallel zero",
			mutate:  func(c *Config) { c.Model.NParallel = 0 },
			wantErr: "model.n_parallel must be >= 1",
		},

		// KV cache types.
		{
			name:    "empty cache_type_k",
			mutate:  func(c *Config) { c.Model.CacheTypeK = "" },
			wantErr: "model.cache_type_k must be one of",
		},
		{
			name:    "unknown cache_type_v",
			mutate:  func(c *Config) { c.Model.CacheTypeV = "q3_k" },
			wantErr: "model.cache_type_v must be one of",
		},
		{
			name:   "f16 cache types accepted",
			mutate: func(c *Config) { c.Model.CacheTypeK = "f16"; c.Model.CacheTypeV = "f16" },
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
				c.Prompt.CtxSize = 4096
				c.Prompt.MemoryTokenBudget = 3000
				c.Prompt.ConversationReserve = 2000
			},
			wantErr: "exceed prompt.ctx_size",
		},
		{
			name: "budget plus reserve equals ctx is fine",
			mutate: func(c *Config) {
				c.Prompt.CtxSize = 8192
				c.Prompt.MemoryTokenBudget = 4096
				c.Prompt.ConversationReserve = 4096
			},
		},
		{
			name: "ctx size zero skips budget check",
			mutate: func(c *Config) {
				c.Prompt.CtxSize = 0
				c.Prompt.MemoryTokenBudget = 1000
				c.Prompt.ConversationReserve = 1000
			},
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
