package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/VrncQuentin/harness/internal/config"
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
	endpointsJSON, err := json.Marshal(d.Endpoints.List)
	if err != nil {
		return fmt.Errorf("db: seed config: marshal endpoints: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT OR IGNORE INTO config (
			id,
			endpoints_json, active_endpoint, active_model,
			embedder_binary, embedder_model_path, embedder_port, embedder_verbose,
			agent_active,
			ui_port, ui_open_on_start, ui_sidebar_recent_sessions,
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
			loop_edit_enabled, loop_exec_enabled,
			loop_go_test_enabled,
			loop_go_lint_enabled,
			loop_git_commit_enabled,
			loop_git_branch_enabled,
			loop_git_checkout_enabled,
			loop_web_search_enabled,
			loop_memory_query_enabled,
			loop_git_push_enabled,
			loop_gh_pr_create_enabled,
			loop_gh_pr_merge_enabled,
			loop_gh_pr_wait_enabled
	) VALUES (
		1,
		?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	)`,		string(endpointsJSON), d.Endpoints.Active, d.Endpoints.ActiveModel,
		d.Embedder.Binary, d.Embedder.ModelPath, d.Embedder.Port, boolInt(d.Embedder.Verbose),
		d.Agent.Active,
		d.UI.Port, boolInt(d.UI.OpenOnStart), d.UI.SidebarRecentSessions,
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
		boolInt(d.Loop.EditEnabled), boolInt(d.Loop.ExecEnabled),
		boolInt(d.Loop.GoTestEnabled),
		boolInt(d.Loop.GoLintEnabled),
		boolInt(d.Loop.GitCommitEnabled),
		boolInt(d.Loop.GitBranchEnabled),
		boolInt(d.Loop.GitCheckoutEnabled),
		boolInt(d.Loop.WebSearchEnabled),
		boolInt(d.Loop.MemoryQueryEnabled),
		boolInt(d.Loop.GitPushEnabled),
		boolInt(d.Loop.GHPRCreateEnabled),
		boolInt(d.Loop.GHPRMergeEnabled),
		boolInt(d.Loop.GHPRWaitEnabled),
	)
	if err != nil {
		return fmt.Errorf("db: seed config: %w", err)
	}
	return nil
}

// Load returns the current config and whether the user has explicitly saved
// it at least once. A fresh install returns (Defaults(), false, nil).
func (s *ConfigStore) Load() (*config.Config, bool, error) {
	var (
		cfg             config.Config
		endpointsJSON   string
		embedderVerbose int
		openOnStart     int
		apiEnabled      int
		savedAt         sql.NullInt64
		readEnabled     int
		fileList        int
		astMap          int
		astFind         int
		gitStatus       int
		gitDiff         int
		gitLog          int
		editEnabled     int
		execEnabled     int
		goTest          int
		goLint          int
		gitCommit       int
		gitBranch       int
		gitCheckout     int
		webSearch       int
		memoryQuery     int
		gitPush         int
		ghPRCreate      int
		ghPRMerge       int
		ghPRWait        int
		prometheusEnabled int
	)
	row := s.db.QueryRow(`
		SELECT
			endpoints_json, active_endpoint, active_model,
			embedder_binary, embedder_model_path, embedder_port, embedder_verbose,
			agent_active,
			ui_port, ui_open_on_start, ui_sidebar_recent_sessions,
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
			loop_edit_enabled, loop_exec_enabled,
			loop_go_test_enabled,
			loop_go_lint_enabled,
			loop_git_commit_enabled,
			loop_git_branch_enabled,
			loop_git_checkout_enabled,
			loop_web_search_enabled,
			loop_memory_query_enabled,
			loop_git_push_enabled,
			loop_gh_pr_create_enabled,
			loop_gh_pr_merge_enabled,
			loop_gh_pr_wait_enabled,
			saved_at
		FROM config WHERE id = 1`)

	err := row.Scan(
		&endpointsJSON, &cfg.Endpoints.Active, &cfg.Endpoints.ActiveModel,
		&cfg.Embedder.Binary, &cfg.Embedder.ModelPath, &cfg.Embedder.Port, &embedderVerbose,
		&cfg.Agent.Active,
		&cfg.UI.Port, &openOnStart, &cfg.UI.SidebarRecentSessions,
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
		&editEnabled, &execEnabled, &goTest, &goLint, &gitCommit, &gitBranch, &gitCheckout, &webSearch,
		&memoryQuery, &gitPush, &ghPRCreate, &ghPRMerge, &ghPRWait,
		&savedAt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("db: load config: %w", err)
	}
	if err := json.Unmarshal([]byte(endpointsJSON), &cfg.Endpoints.List); err != nil {
		return nil, false, fmt.Errorf("db: load config: parse endpoints: %w", err)
	}
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
	cfg.Loop.ExecEnabled = execEnabled != 0
	cfg.Loop.GoTestEnabled = goTest != 0
	cfg.Loop.GoLintEnabled = goLint != 0
	cfg.Loop.GitCommitEnabled = gitCommit != 0
	cfg.Loop.GitBranchEnabled = gitBranch != 0
	cfg.Loop.GitCheckoutEnabled = gitCheckout != 0
	cfg.Loop.WebSearchEnabled = webSearch != 0
	cfg.Loop.MemoryQueryEnabled = memoryQuery != 0
	cfg.Loop.GitPushEnabled = gitPush != 0
	cfg.Loop.GHPRCreateEnabled = ghPRCreate != 0
	cfg.Loop.GHPRMergeEnabled = ghPRMerge != 0
	cfg.Loop.GHPRWaitEnabled = ghPRWait != 0
	cfg.Metrics.PrometheusEnabled = prometheusEnabled != 0
	return &cfg, savedAt.Valid, nil
}

// Save writes cfg and marks the row as user-saved.
func (s *ConfigStore) Save(cfg *config.Config) error {
	if cfg == nil {
		return ErrNilConfig
	}
	endpointsJSON, err := json.Marshal(cfg.Endpoints.List)
	if err != nil {
		return fmt.Errorf("db: save config: marshal endpoints: %w", err)
	}
	_, err = s.db.Exec(`
		UPDATE config SET
			endpoints_json = ?, active_endpoint = ?, active_model = ?,
			embedder_binary = ?, embedder_model_path = ?, embedder_port = ?, embedder_verbose = ?,
			agent_active = ?,
			ui_port = ?, ui_open_on_start = ?, ui_sidebar_recent_sessions = ?,
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
			loop_edit_enabled = ?, loop_exec_enabled = ?,
			loop_go_test_enabled = ?,
			loop_go_lint_enabled = ?,
			loop_git_commit_enabled = ?,
			loop_git_branch_enabled = ?,
			loop_git_checkout_enabled = ?,
			loop_web_search_enabled = ?,
			loop_memory_query_enabled = ?,
			loop_git_push_enabled = ?,
			loop_gh_pr_create_enabled = ?,
			loop_gh_pr_merge_enabled = ?,
			loop_gh_pr_wait_enabled = ?,
			saved_at = ?
		WHERE id = 1`,
		string(endpointsJSON), cfg.Endpoints.Active, cfg.Endpoints.ActiveModel,
		cfg.Embedder.Binary, cfg.Embedder.ModelPath, cfg.Embedder.Port, boolInt(cfg.Embedder.Verbose),
		cfg.Agent.Active,
		cfg.UI.Port, boolInt(cfg.UI.OpenOnStart), cfg.UI.SidebarRecentSessions,
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
		boolInt(cfg.Loop.EditEnabled), boolInt(cfg.Loop.ExecEnabled),
		boolInt(cfg.Loop.GoTestEnabled),
		boolInt(cfg.Loop.GoLintEnabled),
		boolInt(cfg.Loop.GitCommitEnabled),
		boolInt(cfg.Loop.GitBranchEnabled),
		boolInt(cfg.Loop.GitCheckoutEnabled),
		boolInt(cfg.Loop.WebSearchEnabled),
		boolInt(cfg.Loop.MemoryQueryEnabled),
		boolInt(cfg.Loop.GitPushEnabled),
		boolInt(cfg.Loop.GHPRCreateEnabled),
		boolInt(cfg.Loop.GHPRMergeEnabled),
		boolInt(cfg.Loop.GHPRWaitEnabled),
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
