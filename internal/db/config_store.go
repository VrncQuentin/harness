package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vrnc/harness/internal/config"
)

// ErrNilConfig is returned by Save when called with a nil config.
var ErrNilConfig = errors.New("db: save config: nil config")

// ConfigStore persists the single-row harness config.
type ConfigStore struct {
	db *sql.DB
}

var _ config.Store = (*ConfigStore)(nil)

// seed inserts the singleton config row if it doesn't exist, populating every
// column from config.Defaults(). Idempotent via INSERT OR IGNORE. This is the
// single source of truth for initial values - the migration carries no
// column defaults of its own.
func (s *ConfigStore) seed() error {
	d := config.Defaults()
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO config (
			id,
			model_binary, model_path, model_ctx_size, model_gpu_layers,
			model_n_parallel, model_port, model_verbose,
			model_cache_type_k, model_cache_type_v,
			embedder_binary, embedder_model_path, embedder_port, embedder_verbose,
			agent_active,
			ui_port, ui_open_on_start,
			api_enabled, api_port,
			prompt_memory_token_budget, prompt_conversation_reserve,
			prompt_recency_n, prompt_summarizer_prompt,
			prompt_semantic_weight, prompt_recency_weight,
			prompt_promotion_dedup_threshold,
			queue_max_depth,
			metrics_retention_days, metrics_prometheus_enabled,
			log_ring_max_entries, log_proc_max_lines,
			active_project_slug, project_llama_on_switch,
			loop_max_turns, loop_doom_threshold,
			loop_read_enabled, loop_file_list_enabled,
			loop_ast_map_enabled, loop_ast_find_enabled,
			loop_git_status_enabled,
			loop_git_diff_enabled,
			loop_git_log_enabled,
			loop_edit_enabled, loop_shell_exec_enabled,
			loop_web_search_enabled
	) VALUES (
		1,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?
	)`,
		d.Model.Binary, d.Model.ModelPath, d.Model.CtxSize, d.Model.GPULayers,
		d.Model.NParallel, d.Model.Port, boolInt(d.Model.Verbose),
		d.Model.CacheTypeK, d.Model.CacheTypeV,
		d.Embedder.Binary, d.Embedder.ModelPath, d.Embedder.Port, boolInt(d.Embedder.Verbose),
		d.Agent.Active,
		d.UI.Port, boolInt(d.UI.OpenOnStart),
		boolInt(d.API.Enabled), d.API.Port,
		d.Prompt.MemoryTokenBudget, d.Prompt.ConversationReserve,
		d.Prompt.RecencyN, d.Prompt.SummarizerPrompt,
		d.Prompt.SemanticWeight, d.Prompt.RecencyWeight,
		d.Prompt.PromotionDedupThreshold,
		d.Queue.MaxDepth,
		d.Metrics.RetentionDays, boolInt(d.Metrics.PrometheusEnabled),
		d.Log.RingMaxEntries, d.Log.ProcMaxLines,
		d.Project.ActiveProjectSlug, d.Project.LlamaOnSwitch,
		d.Loop.MaxTurns, d.Loop.DoomThreshold,
		boolInt(d.Loop.ReadEnabled), boolInt(d.Loop.FileListEnabled),
		boolInt(d.Loop.AstMapEnabled), boolInt(d.Loop.AstFindEnabled),
		boolInt(d.Loop.GitStatusEnabled),
		boolInt(d.Loop.GitDiffEnabled),
		boolInt(d.Loop.GitLogEnabled),
		boolInt(d.Loop.EditEnabled), boolInt(d.Loop.ShellExecEnabled),
		boolInt(d.Loop.WebSearchEnabled),
	)
	if err != nil {
		return fmt.Errorf("db: seed config: %w", err)
	}
	return nil
}

// Load returns the current config and whether the user has explicitly saved
// it at least once. A fresh install returns (Defaults(), false, nil).
func (s *ConfigStore) Load() (*config.Config, bool, error) {
	row := s.db.QueryRow(`
		SELECT
			model_binary, model_path, model_ctx_size, model_gpu_layers,
			model_n_parallel, model_port, model_verbose,
			model_cache_type_k, model_cache_type_v,
			embedder_binary, embedder_model_path, embedder_port, embedder_verbose,
			agent_active,
			ui_port, ui_open_on_start,
			api_enabled, api_port,
			prompt_memory_token_budget, prompt_conversation_reserve,
			prompt_recency_n, prompt_summarizer_prompt,
			prompt_semantic_weight, prompt_recency_weight,
			prompt_promotion_dedup_threshold,
			queue_max_depth,
			metrics_retention_days, metrics_prometheus_enabled,
			log_ring_max_entries, log_proc_max_lines,
			active_project_slug, project_llama_on_switch,
			loop_max_turns, loop_doom_threshold,
			loop_read_enabled, loop_file_list_enabled,
			loop_ast_map_enabled, loop_ast_find_enabled,
			loop_git_status_enabled,
			loop_git_diff_enabled,
			loop_git_log_enabled,
			loop_edit_enabled, loop_shell_exec_enabled,
			loop_web_search_enabled,
			saved_at
		FROM config WHERE id = 1`)

	var (
		cfg               config.Config
		modelVerbose      int
		embedderVerbose   int
		openOnStart       int
		apiEnabled        int
		savedAt           sql.NullInt64
		readEnabled       int
		fileList          int
		astMap            int
		astFind           int
		gitStatus         int
		gitDiff           int
		gitLog            int
		editEnabled       int
		shellExec         int
		webSearch         int
		prometheusEnabled int
	)
	err := row.Scan(
		&cfg.Model.Binary, &cfg.Model.ModelPath, &cfg.Model.CtxSize, &cfg.Model.GPULayers,
		&cfg.Model.NParallel, &cfg.Model.Port, &modelVerbose,
		&cfg.Model.CacheTypeK, &cfg.Model.CacheTypeV,
		&cfg.Embedder.Binary, &cfg.Embedder.ModelPath, &cfg.Embedder.Port, &embedderVerbose,
		&cfg.Agent.Active,
		&cfg.UI.Port, &openOnStart,
		&apiEnabled, &cfg.API.Port,
		&cfg.Prompt.MemoryTokenBudget, &cfg.Prompt.ConversationReserve,
		&cfg.Prompt.RecencyN, &cfg.Prompt.SummarizerPrompt,
		&cfg.Prompt.SemanticWeight, &cfg.Prompt.RecencyWeight,
		&cfg.Prompt.PromotionDedupThreshold,
		&cfg.Queue.MaxDepth,
		&cfg.Metrics.RetentionDays, &prometheusEnabled,
		&cfg.Log.RingMaxEntries, &cfg.Log.ProcMaxLines,
		&cfg.Project.ActiveProjectSlug, &cfg.Project.LlamaOnSwitch,
		&cfg.Loop.MaxTurns, &cfg.Loop.DoomThreshold,
		&readEnabled, &fileList,
		&astMap, &astFind,
		&gitStatus,
		&gitDiff,
		&gitLog,
		&editEnabled, &shellExec, &webSearch,
		&savedAt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("db: load config: %w", err)
	}
	cfg.Model.Verbose = modelVerbose != 0
	cfg.Embedder.Verbose = embedderVerbose != 0
	cfg.UI.OpenOnStart = openOnStart != 0
	cfg.API.Enabled = apiEnabled != 0
	cfg.Loop.ReadEnabled = readEnabled != 0
	cfg.Loop.FileListEnabled = fileList != 0
	cfg.Loop.AstMapEnabled = astMap != 0
	cfg.Loop.AstFindEnabled = astFind != 0
	cfg.Loop.GitStatusEnabled = gitStatus != 0
	cfg.Loop.GitDiffEnabled = gitDiff != 0
	cfg.Loop.GitLogEnabled = gitLog != 0
	cfg.Loop.EditEnabled = editEnabled != 0
	cfg.Loop.ShellExecEnabled = shellExec != 0
	cfg.Loop.WebSearchEnabled = webSearch != 0
	cfg.Metrics.PrometheusEnabled = prometheusEnabled != 0
	return &cfg, savedAt.Valid, nil
}

// Save writes cfg and marks the row as user-saved.
func (s *ConfigStore) Save(cfg *config.Config) error {
	if cfg == nil {
		return ErrNilConfig
	}
	_, err := s.db.Exec(`
		UPDATE config SET
			model_binary = ?, model_path = ?, model_ctx_size = ?, model_gpu_layers = ?,
			model_n_parallel = ?, model_port = ?, model_verbose = ?,
			model_cache_type_k = ?, model_cache_type_v = ?,
			embedder_binary = ?, embedder_model_path = ?, embedder_port = ?, embedder_verbose = ?,
			agent_active = ?,
			ui_port = ?, ui_open_on_start = ?,
			api_enabled = ?, api_port = ?,
			prompt_memory_token_budget = ?, prompt_conversation_reserve = ?,
			prompt_recency_n = ?, prompt_summarizer_prompt = ?,
			prompt_semantic_weight = ?, prompt_recency_weight = ?,
			prompt_promotion_dedup_threshold = ?,
			queue_max_depth = ?,
			metrics_retention_days = ?, metrics_prometheus_enabled = ?,
			log_ring_max_entries = ?, log_proc_max_lines = ?,
			active_project_slug = ?, project_llama_on_switch = ?,
			loop_max_turns = ?, loop_doom_threshold = ?,
			loop_read_enabled = ?, loop_file_list_enabled = ?,
			loop_ast_map_enabled = ?, loop_ast_find_enabled = ?,
			loop_git_status_enabled = ?,
			loop_git_diff_enabled = ?,
			loop_git_log_enabled = ?,
			loop_edit_enabled = ?, loop_shell_exec_enabled = ?,
			loop_web_search_enabled = ?,
			saved_at = ?
		WHERE id = 1`,
		cfg.Model.Binary, cfg.Model.ModelPath, cfg.Model.CtxSize, cfg.Model.GPULayers,
		cfg.Model.NParallel, cfg.Model.Port, boolInt(cfg.Model.Verbose),
		cfg.Model.CacheTypeK, cfg.Model.CacheTypeV,
		cfg.Embedder.Binary, cfg.Embedder.ModelPath, cfg.Embedder.Port, boolInt(cfg.Embedder.Verbose),
		cfg.Agent.Active,
		cfg.UI.Port, boolInt(cfg.UI.OpenOnStart),
		boolInt(cfg.API.Enabled), cfg.API.Port,
		cfg.Prompt.MemoryTokenBudget, cfg.Prompt.ConversationReserve,
		cfg.Prompt.RecencyN, cfg.Prompt.SummarizerPrompt,
		cfg.Prompt.SemanticWeight, cfg.Prompt.RecencyWeight,
		cfg.Prompt.PromotionDedupThreshold,
		cfg.Queue.MaxDepth,
		cfg.Metrics.RetentionDays, boolInt(cfg.Metrics.PrometheusEnabled),
		cfg.Log.RingMaxEntries, cfg.Log.ProcMaxLines,
		cfg.Project.ActiveProjectSlug, cfg.Project.LlamaOnSwitch,
		cfg.Loop.MaxTurns, cfg.Loop.DoomThreshold,
		boolInt(cfg.Loop.ReadEnabled), boolInt(cfg.Loop.FileListEnabled),
		boolInt(cfg.Loop.AstMapEnabled), boolInt(cfg.Loop.AstFindEnabled),
		boolInt(cfg.Loop.GitStatusEnabled),
		boolInt(cfg.Loop.GitDiffEnabled),
		boolInt(cfg.Loop.GitLogEnabled),
		boolInt(cfg.Loop.EditEnabled), boolInt(cfg.Loop.ShellExecEnabled),
		boolInt(cfg.Loop.WebSearchEnabled),
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: save config: %w", err)
	}
	return nil
}

// SetActiveProjectSlug updates the active-project preference without marking
// first-run configuration complete. Full setup remains the responsibility of
// Save after the required model and embedder fields have been validated.
func (s *ConfigStore) SetActiveProjectSlug(slug string) error {
	if _, err := s.db.Exec(`UPDATE config SET active_project_slug = ? WHERE id = 1`, slug); err != nil {
		return fmt.Errorf("db: set active project slug: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
