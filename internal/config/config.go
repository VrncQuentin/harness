// Package config defines the harness configuration schema, defaults, and
// validation. Persistence lives in internal/db - this package holds no SQL.
package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/vrnc/harness/internal/project"
)

// Sentinel errors returned by Validate for missing required fields.
var (
	ErrModelBinaryRequired       = errors.New("config: model.binary is required")
	ErrModelPathRequired         = errors.New("config: model.model_path is required")
	ErrEmbedderBinaryRequired    = errors.New("config: embedder.binary is required")
	ErrEmbedderPathRequired      = errors.New("config: embedder.model_path is required")
	ErrActiveProjectSlugRequired = errors.New("config: project.active_project_slug is required")
	ErrInvalidLlamaOnSwitch      = errors.New("config: project.llama_on_switch must be keep or reload")
)

// ValidLlamaOnSwitch values for ProjectConfig.LlamaOnSwitch.
var ValidLlamaOnSwitch = []string{"keep", "reload"}

// defaultSummarizerPrompt is the system prompt the M3 session summarizer
// (Track C) feeds to the model when writing an episode. The user can
// override it via the /config page; an empty value tells the summarizer
// to fall back to this string.
const defaultSummarizerPrompt = `You are summarizing a conversation between a user and an AI agent. Produce a concise third-person summary capturing:
- the user's goal or question
- key decisions, code paths, or facts established
- unresolved questions or follow-ups

Write 3-8 short paragraphs in plain markdown. Do not include the conversation verbatim. Do not address the user directly.`

// Config is the top-level configuration structure for the harness.
type Config struct {
	Model    ModelConfig
	Embedder EmbedderConfig
	Agent    AgentConfig
	Project  ProjectConfig
	UI       UIConfig
	API      APIConfig
	Prompt   PromptConfig
	Queue    QueueConfig
	Metrics  MetricsConfig
	Log      LogConfig
	Loop     LoopConfig
}

// ModelConfig holds llama-server configuration.
type ModelConfig struct {
	Binary    string
	ModelPath string
	CtxSize   int
	GPULayers int
	NParallel int
	Port      int
	// Verbose toggles llama-server's --verbose flag. Off by default because
	// it's chatty; turn it on when diagnosing silent startup crashes.
	Verbose bool
	// CacheTypeK and CacheTypeV select the on-GPU dtype for the KV cache,
	// passed through as llama-server's --cache-type-k / --cache-type-v.
	// Default q8_0 cuts KV memory roughly in half versus f16 with negligible
	// quality loss. Quantizing the V cache typically requires --flash-attn
	// in llama-server; we expose that as a separate knob if/when needed.
	CacheTypeK string
	CacheTypeV string
}

// ValidCacheTypes lists the KV cache dtypes llama-server accepts. Used by
// Validate and surfaced to the UI as a select. Order is meaningful: f16
// first (the safe default if a user clears the field), then descending
// precision.
var ValidCacheTypes = []string{
	"f32", "f16", "bf16",
	"q8_0",
	"q5_0", "q5_1",
	"q4_0", "q4_1",
	"iq4_nl",
}

// EmbedderConfig holds the embedder sidecar configuration.
type EmbedderConfig struct {
	Binary    string
	ModelPath string
	Port      int
	// Verbose toggles the embedder sidecar's --verbose flag. Same rationale
	// as ModelConfig.Verbose.
	Verbose bool
}

// AgentConfig tracks the currently active agent. An empty Active means
// no agent is selected; the prompt assembler skips the persona/notes
// layers in that case.
type AgentConfig struct {
	Active string
}

// ProjectConfig holds project scoping and switch behavior.
type ProjectConfig struct {
	ActiveProjectSlug string
	LlamaOnSwitch     string
}

// UIConfig holds UI server configuration.
type UIConfig struct {
	Port        int
	OpenOnStart bool
}

// APIConfig holds the optional API server configuration.
type APIConfig struct {
	Enabled bool
	Port    int
}

// PromptConfig holds prompt assembly configuration.
type PromptConfig struct {
	CtxSize             int
	MemoryTokenBudget   int
	ConversationReserve int
	// RecencyN caps the number of most-recent episodes injected by the
	// assembler. <= 0 means unlimited (the memory budget remains the
	// hard ceiling).
	RecencyN int
	// SummarizerPrompt is the system prompt the session summarizer feeds
	// to the model when writing an episode. An empty string lets the
	// summarizer fall back to its built-in default.
	SummarizerPrompt string
	// SemanticWeight controls the influence of semantic similarity in
	// blended episode retrieval. 0 means pure recency.
	SemanticWeight float64
	// RecencyWeight controls the influence of recency in blended episode
	// retrieval. 0 means pure semantic search.
	RecencyWeight float64
	// PromotionDedupThreshold is the cosine similarity threshold for
	// blocking near-duplicate fact promotions. 0 disables dedup entirely;
	// 0.95 is recommended for normal use.
	PromotionDedupThreshold float64
}

// QueueConfig holds queue configuration.
type QueueConfig struct {
	MaxDepth int
	WALPath  string
}

// MetricsConfig holds metrics retention configuration. The database file
// itself is the shared harness.db next to the binary, not configurable here.
type MetricsConfig struct {
	RetentionDays     int
	PrometheusEnabled bool
}

// LogConfig holds in-memory log buffer sizes. Both buffers are allocated
// once at startup, so changes take effect on the next harness launch.
type LogConfig struct {
	// RingMaxEntries caps the harness log ring that feeds the status page
	// and the harness-log channel of the /events SSE stream.
	RingMaxEntries int
	// ProcMaxLines caps each child process's stdout+stderr tail shown on
	// the status page.
	ProcMaxLines int
}

// LoopConfig holds native agent loop configuration.
type LoopConfig struct {
	// MaxTurns caps the number of loop iterations (model call + tool
	// dispatch) before the loop terminates with a limit error.
	MaxTurns int
	// DoomThreshold is the number of consecutive identical tool calls
	// (same tool id + same args JSON) the loop tolerates before
	// terminating with a doom-loop error.
	DoomThreshold int
	// FileReadEnabled toggles the file_read tool. When false the model
	// receives a tool-not-available result instead.
	FileReadEnabled bool
	// FileListEnabled toggles the file_list tool.
	FileListEnabled bool
	// FileWriteEnabled toggles the file_write tool. Off by default; requires
	// the M7 approval layer before it can be enabled safely.
	FileWriteEnabled bool
	// ShellExecEnabled toggles the shell_exec tool. Off by default; requires
	// the M7 approval layer before it can be enabled safely.
	ShellExecEnabled bool
	// WebSearchEnabled toggles the web_search tool. Off by default because it
	// sends the user's query over the network.
	WebSearchEnabled bool
}

// Store persists and retrieves Config. The concrete implementation lives in
// internal/db; callers accept this interface so they can be tested with
// in-memory fakes.
type Store interface {
	Load() (*Config, bool, error)
	Save(*Config) error
}

// Defaults returns a Config with sensible defaults applied. This is the
// single source of truth for initial values; the db package seeds every
// column from these on first run.
func Defaults() Config {
	return Config{
		Model: ModelConfig{
			CtxSize:    32768,
			GPULayers:  -1,
			NParallel:  1,
			Port:       8081,
			CacheTypeK: "q8_0",
			CacheTypeV: "q8_0",
		},
		Embedder: EmbedderConfig{
			Port: 8082,
		},
		UI: UIConfig{
			Port:        3000,
			OpenOnStart: true,
		},
		API: APIConfig{
			Enabled: false,
			Port:    8080,
		},
		Prompt: PromptConfig{
			CtxSize:                 32768,
			MemoryTokenBudget:       6144,
			ConversationReserve:     8192,
			RecencyN:                5,
			SummarizerPrompt:        defaultSummarizerPrompt,
			SemanticWeight:          0.5,
			RecencyWeight:           0.5,
			PromotionDedupThreshold: 0.95,
		},
		Queue: QueueConfig{
			MaxDepth: 8,
		},
		Metrics: MetricsConfig{
			RetentionDays:     30,
			PrometheusEnabled: false,
		},
		Log: LogConfig{
			RingMaxEntries: 500,
			ProcMaxLines:   64,
		},
		Loop: LoopConfig{
			MaxTurns:         10,
			DoomThreshold:    3,
			FileReadEnabled:  true,
			FileListEnabled:  true,
			FileWriteEnabled: false,
			ShellExecEnabled: false,
			WebSearchEnabled: false,
		},
		Project: ProjectConfig{
			ActiveProjectSlug: "global",
			LlamaOnSwitch:     "reload",
		},
	}
}

// Validate checks required fields, numeric bounds, and port collisions. The
// UI trims form values before calling this, but Validate re-checks trimmed
// length so direct callers (tests, future API callers) can't bypass it.
func Validate(cfg *Config) error {
	if strings.TrimSpace(cfg.Model.Binary) == "" {
		return ErrModelBinaryRequired
	}
	if strings.TrimSpace(cfg.Model.ModelPath) == "" {
		return ErrModelPathRequired
	}
	if strings.TrimSpace(cfg.Embedder.Binary) == "" {
		return ErrEmbedderBinaryRequired
	}
	if strings.TrimSpace(cfg.Embedder.ModelPath) == "" {
		return ErrEmbedderPathRequired
	}

	if err := validatePort("model.port", cfg.Model.Port); err != nil {
		return err
	}
	if err := validatePort("embedder.port", cfg.Embedder.Port); err != nil {
		return err
	}
	if err := validatePort("ui.port", cfg.UI.Port); err != nil {
		return err
	}
	if err := validatePort("api.port", cfg.API.Port); err != nil {
		return err
	}

	// Port collisions: check all four regardless of API.Enabled so flipping
	// the flag on later can't silently introduce a conflict.
	seen := make(map[int]string, 4)
	for _, p := range []struct {
		name string
		val  int
	}{
		{"model.port", cfg.Model.Port},
		{"embedder.port", cfg.Embedder.Port},
		{"ui.port", cfg.UI.Port},
		{"api.port", cfg.API.Port},
	} {
		if other, ok := seen[p.val]; ok {
			return fmt.Errorf("config: %s and %s both use port %d", other, p.name, p.val)
		}
		seen[p.val] = p.name
	}

	if cfg.Model.CtxSize < 0 {
		return fmt.Errorf("config: model.ctx_size must be >= 0, got %d", cfg.Model.CtxSize)
	}
	if cfg.Model.GPULayers < -1 {
		return fmt.Errorf("config: model.gpu_layers must be >= -1 (-1 offloads all), got %d", cfg.Model.GPULayers)
	}
	if cfg.Model.NParallel < 1 {
		return fmt.Errorf("config: model.n_parallel must be >= 1, got %d", cfg.Model.NParallel)
	}
	if err := validateCacheType("model.cache_type_k", cfg.Model.CacheTypeK); err != nil {
		return err
	}
	if err := validateCacheType("model.cache_type_v", cfg.Model.CacheTypeV); err != nil {
		return err
	}

	if cfg.Prompt.CtxSize < 0 {
		return fmt.Errorf("config: prompt.ctx_size must be >= 0, got %d", cfg.Prompt.CtxSize)
	}
	if cfg.Prompt.MemoryTokenBudget < 0 {
		return fmt.Errorf("config: prompt.memory_token_budget must be >= 0, got %d", cfg.Prompt.MemoryTokenBudget)
	}
	if cfg.Prompt.ConversationReserve < 0 {
		return fmt.Errorf("config: prompt.conversation_reserve must be >= 0, got %d", cfg.Prompt.ConversationReserve)
	}
	if cfg.Prompt.CtxSize > 0 && cfg.Prompt.MemoryTokenBudget+cfg.Prompt.ConversationReserve > cfg.Prompt.CtxSize {
		return fmt.Errorf("config: prompt.memory_token_budget (%d) + prompt.conversation_reserve (%d) exceed prompt.ctx_size (%d)",
			cfg.Prompt.MemoryTokenBudget, cfg.Prompt.ConversationReserve, cfg.Prompt.CtxSize)
	}
	if cfg.Prompt.RecencyN < 0 {
		return fmt.Errorf("config: prompt.recency_n must be >= 0 (0 means unlimited), got %d", cfg.Prompt.RecencyN)
	}

	if cfg.Queue.MaxDepth < 1 {
		return fmt.Errorf("config: queue.max_depth must be >= 1, got %d", cfg.Queue.MaxDepth)
	}
	if cfg.Metrics.RetentionDays < 1 {
		return fmt.Errorf("config: metrics.retention_days must be >= 1, got %d", cfg.Metrics.RetentionDays)
	}

	if cfg.Log.RingMaxEntries < 1 {
		return fmt.Errorf("config: log.ring_max_entries must be >= 1, got %d", cfg.Log.RingMaxEntries)
	}
	if cfg.Log.ProcMaxLines < 1 {
		return fmt.Errorf("config: log.proc_max_lines must be >= 1, got %d", cfg.Log.ProcMaxLines)
	}

	if cfg.Loop.MaxTurns < 1 {
		return fmt.Errorf("config: loop.max_turns must be >= 1, got %d", cfg.Loop.MaxTurns)
	}
	if cfg.Loop.DoomThreshold < 1 {
		return fmt.Errorf("config: loop.doom_threshold must be >= 1, got %d", cfg.Loop.DoomThreshold)
	}

	if strings.TrimSpace(cfg.Project.ActiveProjectSlug) == "" {
		return ErrActiveProjectSlugRequired
	}
	if err := project.ValidateSlug(cfg.Project.ActiveProjectSlug); err != nil {
		return fmt.Errorf("config: project.active_project_slug: %w", err)
	}
	if !slices.Contains(ValidLlamaOnSwitch, cfg.Project.LlamaOnSwitch) {
		return ErrInvalidLlamaOnSwitch
	}

	return nil
}

func validatePort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("config: %s must be between 1 and 65535, got %d", name, port)
	}
	return nil
}

func validateCacheType(name, value string) error {
	if slices.Contains(ValidCacheTypes, value) {
		return nil
	}
	return fmt.Errorf("config: %s must be one of %s, got %q", name, strings.Join(ValidCacheTypes, ", "), value)
}

// EffectiveModel returns the effective model config for the given project.
// Per-project overrides take precedence; nil values fall back to the global
// defaults in cfg.
func EffectiveModel(cfg *Config, proj *project.Project) ModelConfig {
	m := cfg.Model
	if proj == nil {
		return m
	}
	if proj.ModelBinary != nil {
		m.Binary = *proj.ModelBinary
	}
	if proj.ModelPath != nil {
		m.ModelPath = *proj.ModelPath
	}
	if proj.ModelCtxSize != nil {
		m.CtxSize = *proj.ModelCtxSize
	}
	if proj.ModelGPULayers != nil {
		m.GPULayers = *proj.ModelGPULayers
	}
	if proj.ModelNParallel != nil {
		m.NParallel = *proj.ModelNParallel
	}
	return m
}

// ModelConfigEqual returns true when a and b are identical model
// configurations. Port and Verbose are deliberately excluded — they are
// process-level flags, not model identity fields. Two projects that differ
// only in Port cannot share a llama-server, but since the harness runs
// exactly one llama-server whose Port comes from the global config (never
// project overrides), comparing other fields suffices.
func ModelConfigEqual(a, b ModelConfig) bool {
	return a.Binary == b.Binary &&
		a.ModelPath == b.ModelPath &&
		a.CtxSize == b.CtxSize &&
		a.GPULayers == b.GPULayers &&
		a.NParallel == b.NParallel &&
		a.CacheTypeK == b.CacheTypeK &&
		a.CacheTypeV == b.CacheTypeV
}
