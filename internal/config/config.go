// Package config defines the harness configuration schema, defaults, and
// validation. Persistence lives in internal/db - this package holds no SQL.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/VrncQuentin/harness/internal/project"
	"github.com/VrncQuentin/harness/internal/summarizerprompt"
	"github.com/VrncQuentin/harness/internal/tools"
)

// Sentinel errors returned by Validate for missing required fields.
var (
	ErrModelBinaryRequired       = errors.New("config: model.binary is required")
	ErrModelPathRequired         = errors.New("config: model.model_path is required")
	ErrEmbedderBinaryRequired    = errors.New("config: embedder.binary is required")
	ErrEmbedderPathRequired      = errors.New("config: embedder.model_path is required")
	ErrActiveProjectSlugRequired = errors.New("config: project.active_project_slug is required")
	ErrInvalidLlamaOnSwitch      = errors.New("config: project.llama_on_switch must be keep or reload")
	ErrNoEndpoints               = errors.New("config: at least one model endpoint is required")
	ErrEndpointIDRequired        = errors.New("config: endpoint id is required")
	ErrInvalidEndpointKind       = errors.New("config: endpoint kind must be local or openai")
	ErrBaseURLRequired           = errors.New("config: endpoint base_url is required")
	ErrInvalidBaseURL            = errors.New("config: endpoint base_url must be an http(s) URL")
	ErrEndpointModelRequired     = errors.New("config: endpoint needs at least one model")
	ErrActiveEndpointUnknown     = errors.New("config: active endpoint does not exist")
	ErrActiveModelUnknown        = errors.New("config: active model does not exist in the active endpoint")
)

// Endpoint kinds. A "local" endpoint is the llama-server the harness spawns
// itself; an "openai" endpoint is any external OpenAI-compatible HTTP backend
// (another llama-server already running, Ollama, a hosted API).
const (
	EndpointKindLocal = "local"
	EndpointKindOpenAI = "openai"
)

// ValidEndpointKinds lists the endpoint kinds Validate accepts.
var ValidEndpointKinds = []string{EndpointKindLocal, EndpointKindOpenAI}

// defaultExternalCtxSize is the context-size ceiling assumed for an external
// model that does not declare one. It feeds prompt-budget arithmetic only and
// never configures a process.
const defaultExternalCtxSize = 32768

// ValidLlamaOnSwitch values for ProjectConfig.LlamaOnSwitch.
var ValidLlamaOnSwitch = []string{"keep", "reload"}

// Config is the top-level configuration structure for the harness.
type Config struct {
	Endpoints EndpointsConfig
	Embedder  EmbedderConfig
	Agent     AgentConfig
	Project   ProjectConfig
	UI        UIConfig
	API       APIConfig
	Prompt    PromptConfig
	Queue     QueueConfig
	Metrics   MetricsConfig
	Log       LogConfig
	Loop      LoopConfig
}

// EndpointsConfig is the ordered list of model endpoints and the active
// selection. The active endpoint drives every completion request; a local
// endpoint also owns the llama-server process the harness spawns.
type EndpointsConfig struct {
	// Active is the id of the endpoint used for text completion.
	Active string
	// ActiveModel is the model id selected within the active endpoint. It is
	// empty for a local endpoint, whose loaded model is the only model.
	ActiveModel string
	// List holds every configured endpoint, in display order.
	List []Endpoint
}

// Endpoint is one model backend.
type Endpoint struct {
	ID   string
	Kind string // EndpointKindLocal or EndpointKindOpenAI
	// Name is the display name; empty falls back to ID.
	Name string

	// Local llama-server fields (Kind == EndpointKindLocal).
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

	// External OpenAI-compatible fields (Kind == EndpointKindOpenAI).
	BaseURL string
	APIKey  string
	// Models lists the model ids this endpoint can serve. A hosted API or an
	// Ollama instance typically advertises several.
	Models []EndpointModel
}

// EndpointModel is one model id served by an OpenAI-compatible endpoint.
type EndpointModel struct {
	ID   string
	Name string
	// CtxSize is the context-size ceiling used for prompt-budget arithmetic.
	// 0 falls back to the external default.
	CtxSize int
}

// ModelConfig is the resolved active model a generation talks to. It is a
// projection of the active endpoint plus the selected model, not a stored
// section: local endpoints contribute llama-server fields, external endpoints
// contribute the base URL, optional API key, and model id.
type ModelConfig struct {
	// Kind is EndpointKindLocal or EndpointKindOpenAI.
	Kind string
	// EndpointID and EndpointName identify the source endpoint.
	EndpointID   string
	EndpointName string
	// ModelID and ModelName identify the selected model on an external
	// endpoint; they are empty for a local endpoint.
	ModelID   string
	ModelName string

	// Local llama-server fields (Kind == EndpointKindLocal).
	Binary    string
	ModelPath string
	CtxSize   int
	GPULayers int
	NParallel int
	Port      int
	Verbose   bool
	CacheTypeK string
	CacheTypeV string

	// External endpoint fields (Kind == EndpointKindOpenAI).
	BaseURL string
	APIKey  string
}

// ActiveEndpoint returns the active endpoint, or nil when the selection does
// not reference a configured endpoint.
func (c *Config) ActiveEndpoint() *Endpoint {
	return c.Endpoint(c.Endpoints.Active)
}

// Endpoint returns the endpoint with the given id, or nil.
func (c *Config) Endpoint(id string) *Endpoint {
	for i := range c.Endpoints.List {
		if c.Endpoints.List[i].ID == id {
			return &c.Endpoints.List[i]
		}
	}
	return nil
}

// Model returns the model with the given id on this endpoint, or nil.
func (e *Endpoint) Model(id string) *EndpointModel {
	for i := range e.Models {
		if e.Models[i].ID == id {
			return &e.Models[i]
		}
	}
	return nil
}

// ActiveModelConfig resolves the active endpoint and its selected model into a
// concrete ModelConfig. A zero value is returned when no active endpoint
// resolves. For an external endpoint the selected model is used, falling back
// to the first declared model when ActiveModel does not match.
func (c *Config) ActiveModelConfig() ModelConfig {
	e := c.ActiveEndpoint()
	if e == nil {
		return ModelConfig{}
	}
	m := ModelConfig{
		Kind:         e.Kind,
		EndpointID:   e.ID,
		EndpointName: e.Name,
	}
	if e.Kind == EndpointKindOpenAI {
		m.BaseURL = e.BaseURL
		m.APIKey = e.APIKey
		mm := e.Model(c.Endpoints.ActiveModel)
		if mm == nil && len(e.Models) > 0 {
			mm = &e.Models[0]
		}
		if mm != nil {
			m.ModelID = mm.ID
			m.ModelName = mm.Name
			m.CtxSize = mm.CtxSize
		}
		if m.CtxSize <= 0 {
			m.CtxSize = defaultExternalCtxSize
		}
		return m
	}
	m.Binary = e.Binary
	m.ModelPath = e.ModelPath
	m.CtxSize = e.CtxSize
	m.GPULayers = e.GPULayers
	m.NParallel = e.NParallel
	m.Port = e.Port
	m.Verbose = e.Verbose
	m.CacheTypeK = e.CacheTypeK
	m.CacheTypeV = e.CacheTypeV
	return m
}

// IsExternalModel reports whether the active endpoint is an external
// OpenAI-compatible backend rather than a harness-spawned llama-server.
func (c *Config) IsExternalModel() bool {
	e := c.ActiveEndpoint()
	return e != nil && e.Kind == EndpointKindOpenAI
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
	// SidebarRecentSessions is how many recent saved sessions the project
	// sidebar lists per project. 0 hides the session lists entirely.
	SidebarRecentSessions int
}

// APIConfig holds the optional API server configuration.
type APIConfig struct {
	Enabled bool
	Port    int
}

// PromptConfig holds prompt assembly configuration.
type PromptConfig struct {
	// CtxSize is derived at runtime from the effective model context size.
	// It is intentionally not persisted or user-editable.
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
	// ReadEnabled toggles the read tool. When false the model receives a
	// tool-not-available result instead.
	ReadEnabled bool
	// FileListEnabled toggles the file_list tool.
	FileListEnabled bool
	// AstMapEnabled toggles the ast_map tool (parser-backed file outline).
	AstMapEnabled bool
	// AstFindEnabled toggles the ast_find tool (symbol/content locate).
	AstFindEnabled bool
	// GitStatusEnabled toggles the git_status tool (worktree status, read-only).
	GitStatusEnabled bool
	// GitDiffEnabled toggles the git_diff tool (unified diff, read-only).
	GitDiffEnabled bool
	// GitLogEnabled toggles the git_log tool (commit log, read-only).
	GitLogEnabled bool
	// EditEnabled toggles the edit tool. Off by default; requires the
	// approval layer before it can be enabled safely.
	EditEnabled bool
	// ExecEnabled toggles the exec tool. Off by default; requires
	// the approval layer before it can be enabled safely.
	ExecEnabled bool
	// GoTestEnabled toggles the go_test tool. Off by default; runs the test
	// suite which executes code.
	GoTestEnabled bool
	// GoLintEnabled toggles the go_lint tool. Off by default; requires
	// golangci-lint to be installed.
	GoLintEnabled bool
	// GitCommitEnabled toggles the git_commit tool. Off by default; requires
	// approval — commits to workspace repos, scope-checked against memory repos.
	GitCommitEnabled bool
	// GitBranchEnabled toggles the git_branch tool. Off by default; requires
	// approval — creates branches in workspace repos, scope-checked against memory repos.
	GitBranchEnabled bool
	// GitCheckoutEnabled toggles the git_checkout tool. Off by default; requires
	// approval — switches branches in workspace repos, scope-checked against memory repos.
	GitCheckoutEnabled bool
	// WebSearchEnabled toggles the web_search tool. Off by default because it
	// sends the user's query over the network.
	WebSearchEnabled bool
	// MemoryQueryEnabled toggles the memory_query tool. Off by default; requires
	// the embedder to be running and the episode index to be populated.
	MemoryQueryEnabled bool
	// GitPushEnabled toggles the git_push proposal tool. Off by default.
	// When enabled the tool returns a push proposal for human execution,
	// never pushes autonomously.
	GitPushEnabled bool
	// GHPRCreateEnabled toggles the gh_pr_create proposal tool. Off by default.
	GHPRCreateEnabled bool
	// GHPRMergeEnabled toggles the gh_pr_merge proposal tool. Off by default.
	GHPRMergeEnabled bool
	// GHPRWaitEnabled toggles the gh_pr_wait CI poller. Off by default because
	// it uses the network and may block for up to the wait ceiling.
	GHPRWaitEnabled bool
}

// ToolEnabled reports whether the named built-in tool is enabled by this
// config. Unknown tools are disabled.
func (c LoopConfig) ToolEnabled(id string) bool {
	switch id {
	case "read":
		return c.ReadEnabled
	case "file_list":
		return c.FileListEnabled
	case "ast_map":
		return c.AstMapEnabled
	case "ast_find":
		return c.AstFindEnabled
	case "git_status":
		return c.GitStatusEnabled
	case "git_diff":
		return c.GitDiffEnabled
	case "git_log":
		return c.GitLogEnabled
	case "edit":
		return c.EditEnabled
	case "exec":
		return c.ExecEnabled
	case "go_test":
		return c.GoTestEnabled
	case "go_lint":
		return c.GoLintEnabled
	case "git_commit":
		return c.GitCommitEnabled
	case "git_branch":
		return c.GitBranchEnabled
	case "git_checkout":
		return c.GitCheckoutEnabled
	case "web_search":
		return c.WebSearchEnabled
	case "memory_query":
		return c.MemoryQueryEnabled
	case "git_push":
		return c.GitPushEnabled
	case "gh_pr_create":
		return c.GHPRCreateEnabled
	case "gh_pr_merge":
		return c.GHPRMergeEnabled
	case "gh_pr_wait":
		return c.GHPRWaitEnabled
	default:
		return false
	}
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
	const localID = "local"
	return Config{
		Endpoints: EndpointsConfig{
			Active: localID,
			List: []Endpoint{{
				ID:         localID,
				Kind:       EndpointKindLocal,
				Name:       "Local llama-server",
				CtxSize:    32768,
				GPULayers:  -1,
				NParallel:  1,
				Port:       8081,
				CacheTypeK: "q8_0",
				CacheTypeV: "q8_0",
			}},
		},
		Embedder: EmbedderConfig{
			Port: 8082,
		},
		UI: UIConfig{
			Port:                  3000,
			OpenOnStart:           true,
			SidebarRecentSessions: 5,
		},
		API: APIConfig{
			Enabled: false,
			Port:    8080,
		},
		Prompt: PromptConfig{
			MemoryTokenBudget:       6144,
			ConversationReserve:     8192,
			RecencyN:                5,
			SummarizerPrompt:        summarizerprompt.Default,
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
			MaxTurns:           10,
			DoomThreshold:      3,
			ReadEnabled:        tools.BuiltinDefaultEnabled("read"),
			FileListEnabled:    tools.BuiltinDefaultEnabled("file_list"),
			AstMapEnabled:      tools.BuiltinDefaultEnabled("ast_map"),
			AstFindEnabled:     tools.BuiltinDefaultEnabled("ast_find"),
			GitStatusEnabled:   tools.BuiltinDefaultEnabled("git_status"),
			GitDiffEnabled:     tools.BuiltinDefaultEnabled("git_diff"),
			GitLogEnabled:      tools.BuiltinDefaultEnabled("git_log"),
			EditEnabled:        tools.BuiltinDefaultEnabled("edit"),
			ExecEnabled:        tools.BuiltinDefaultEnabled("exec"),
			GoTestEnabled:      tools.BuiltinDefaultEnabled("go_test"),
			GoLintEnabled:      tools.BuiltinDefaultEnabled("go_lint"),
			GitCommitEnabled:   tools.BuiltinDefaultEnabled("git_commit"),
			GitBranchEnabled:   tools.BuiltinDefaultEnabled("git_branch"),
			GitCheckoutEnabled: tools.BuiltinDefaultEnabled("git_checkout"),
			WebSearchEnabled:   tools.BuiltinDefaultEnabled("web_search"),
			MemoryQueryEnabled: tools.BuiltinDefaultEnabled("memory_query"),
			GitPushEnabled:     tools.BuiltinDefaultEnabled("git_push"),
			GHPRCreateEnabled:  tools.BuiltinDefaultEnabled("gh_pr_create"),
			GHPRMergeEnabled:   tools.BuiltinDefaultEnabled("gh_pr_merge"),
			GHPRWaitEnabled:    tools.BuiltinDefaultEnabled("gh_pr_wait"),
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
	if len(cfg.Endpoints.List) == 0 {
		return ErrNoEndpoints
	}

	seen := make(map[string]bool, len(cfg.Endpoints.List))
	localPorts := make(map[int]string)
	for i := range cfg.Endpoints.List {
		e := &cfg.Endpoints.List[i]
		id := strings.TrimSpace(e.ID)
		if id == "" {
			return ErrEndpointIDRequired
		}
		e.ID = id
		if seen[id] {
			return fmt.Errorf("config: duplicate endpoint id %q", id)
		}
		seen[id] = true

		switch e.Kind {
		case EndpointKindLocal:
			if err := validateLocalEndpoint(e); err != nil {
				return err
			}
			if other, ok := localPorts[e.Port]; ok {
				return fmt.Errorf("config: endpoints %q and %q both use port %d", other, e.ID, e.Port)
			}
			localPorts[e.Port] = e.ID
		case EndpointKindOpenAI:
			if err := validateOpenAIEndpoint(e); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s, got %q", ErrInvalidEndpointKind, e.Kind)
		}
	}

	active := cfg.ActiveEndpoint()
	if active == nil {
		return fmt.Errorf("config: active endpoint %q does not exist", cfg.Endpoints.Active)
	}
	if active.Kind == EndpointKindOpenAI && active.Model(cfg.Endpoints.ActiveModel) == nil {
		return fmt.Errorf("config: active model %q does not exist in endpoint %q", cfg.Endpoints.ActiveModel, active.ID)
	}

	if strings.TrimSpace(cfg.Embedder.Binary) == "" {
		return ErrEmbedderBinaryRequired
	}
	if strings.TrimSpace(cfg.Embedder.ModelPath) == "" {
		return ErrEmbedderPathRequired
	}

	if err := validatePort("embedder.port", cfg.Embedder.Port); err != nil {
		return err
	}
	if err := validatePort("ui.port", cfg.UI.Port); err != nil {
		return err
	}
	if cfg.UI.SidebarRecentSessions < 0 || cfg.UI.SidebarRecentSessions > 10 {
		return fmt.Errorf("config: ui.sidebar_recent_sessions must be between 0 and 10, got %d", cfg.UI.SidebarRecentSessions)
	}
	if err := validatePort("api.port", cfg.API.Port); err != nil {
		return err
	}

	// Port collisions: check all local-endpoint ports against each other and
	// against the fixed embedder/ui/api ports regardless of API.Enabled so
	// flipping the flag on later can't silently introduce a conflict.
	used := make(map[int]string, 4+len(localPorts))
	for _, p := range []struct {
		name string
		val  int
	}{
		{"embedder.port", cfg.Embedder.Port},
		{"ui.port", cfg.UI.Port},
		{"api.port", cfg.API.Port},
	} {
		if other, ok := used[p.val]; ok {
			return fmt.Errorf("config: %s and %s both use port %d", other, p.name, p.val)
		}
		used[p.val] = p.name
	}
	for port, id := range localPorts {
		if other, ok := used[port]; ok {
			return fmt.Errorf("config: endpoint %q and %s both use port %d", id, other, port)
		}
		used[port] = "endpoint " + id
	}

	if cfg.Prompt.MemoryTokenBudget < 0 {
		return fmt.Errorf("config: prompt.memory_token_budget must be >= 0, got %d", cfg.Prompt.MemoryTokenBudget)
	}
	if cfg.Prompt.ConversationReserve < 0 {
		return fmt.Errorf("config: prompt.conversation_reserve must be >= 0, got %d", cfg.Prompt.ConversationReserve)
	}
	activeCtx := cfg.ActiveModelConfig().CtxSize
	if activeCtx > 0 && cfg.Prompt.MemoryTokenBudget+cfg.Prompt.ConversationReserve > activeCtx {
		return fmt.Errorf("config: prompt.memory_token_budget (%d) + prompt.conversation_reserve (%d) exceed model.ctx_size (%d)",
			cfg.Prompt.MemoryTokenBudget, cfg.Prompt.ConversationReserve, activeCtx)
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

func validateLocalEndpoint(e *Endpoint) error {
	if strings.TrimSpace(e.Binary) == "" {
		return ErrModelBinaryRequired
	}
	if strings.TrimSpace(e.ModelPath) == "" {
		return ErrModelPathRequired
	}
	if err := validatePort("model.port", e.Port); err != nil {
		return err
	}
	if e.CtxSize < 0 {
		return fmt.Errorf("config: model.ctx_size must be >= 0, got %d", e.CtxSize)
	}
	if e.GPULayers < -1 {
		return fmt.Errorf("config: model.gpu_layers must be >= -1 (-1 offloads all), got %d", e.GPULayers)
	}
	if e.NParallel < 1 {
		return fmt.Errorf("config: model.n_parallel must be >= 1, got %d", e.NParallel)
	}
	if err := validateCacheType("model.cache_type_k", e.CacheTypeK); err != nil {
		return err
	}
	if err := validateCacheType("model.cache_type_v", e.CacheTypeV); err != nil {
		return err
	}
	return nil
}

func validateOpenAIEndpoint(e *Endpoint) error {
	base := strings.TrimSpace(e.BaseURL)
	if base == "" {
		return ErrBaseURLRequired
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidBaseURL
	}
	if len(e.Models) == 0 {
		return ErrEndpointModelRequired
	}
	seen := make(map[string]bool, len(e.Models))
	for i := range e.Models {
		id := strings.TrimSpace(e.Models[i].ID)
		if id == "" {
			return fmt.Errorf("config: endpoint %q model id is required", e.ID)
		}
		e.Models[i].ID = id
		if seen[id] {
			return fmt.Errorf("config: endpoint %q duplicate model id %q", e.ID, id)
		}
		seen[id] = true
		if e.Models[i].CtxSize < 0 {
			return fmt.Errorf("config: endpoint %q model %q ctx_size must be >= 0", e.ID, id)
		}
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
// Per-project overrides take precedence over the active local endpoint; nil
// values fall back to the endpoint's fields. Overrides apply only to a local
// endpoint — an external backend's base URL and model selection are never
// overridden per project.
func EffectiveModel(cfg *Config, proj *project.Project) ModelConfig {
	m := cfg.ActiveModelConfig()
	if m.Kind != EndpointKindLocal || proj == nil {
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
// configurations. Comparison is kind-aware: local endpoints compare llama-server
// identity fields (Port and Verbose are deliberately excluded — they are
// process-level flags, not model identity), external endpoints compare the base
// URL, API key, model id, and context size.
func ModelConfigEqual(a, b ModelConfig) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == EndpointKindOpenAI {
		return a.BaseURL == b.BaseURL &&
			a.APIKey == b.APIKey &&
			a.ModelID == b.ModelID &&
			a.CtxSize == b.CtxSize
	}
	return a.Binary == b.Binary &&
		a.ModelPath == b.ModelPath &&
		a.CtxSize == b.CtxSize &&
		a.GPULayers == b.GPULayers &&
		a.NParallel == b.NParallel &&
		a.CacheTypeK == b.CacheTypeK &&
		a.CacheTypeV == b.CacheTypeV
}
