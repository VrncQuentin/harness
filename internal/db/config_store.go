package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/vrnc/harness/internal/config"
)

// ConfigStore persists the single-row harness config. It satisfies
// config.Store.
type ConfigStore struct {
	db *sql.DB
}

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
			model_n_parallel, model_port,
			embedder_binary, embedder_model_path, embedder_port,
			memory_repo_path,
			ui_port, ui_open_on_start,
			api_enabled, api_port,
			prompt_ctx_size, prompt_memory_token_budget, prompt_conversation_reserve,
			queue_max_depth, queue_wal_path,
			metrics_retention_days
		) VALUES (
			1,
			?, ?, ?, ?,
			?, ?,
			?, ?, ?,
			?,
			?, ?,
			?, ?,
			?, ?, ?,
			?, ?,
			?
		)`,
		d.Model.Binary, d.Model.ModelPath, d.Model.CtxSize, d.Model.GPULayers,
		d.Model.NParallel, d.Model.Port,
		d.Embedder.Binary, d.Embedder.ModelPath, d.Embedder.Port,
		d.Memory.RepoPath,
		d.UI.Port, boolInt(d.UI.OpenOnStart),
		boolInt(d.API.Enabled), d.API.Port,
		d.Prompt.CtxSize, d.Prompt.MemoryTokenBudget, d.Prompt.ConversationReserve,
		d.Queue.MaxDepth, d.Queue.WALPath,
		d.Metrics.RetentionDays,
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
			model_n_parallel, model_port,
			embedder_binary, embedder_model_path, embedder_port,
			memory_repo_path,
			ui_port, ui_open_on_start,
			api_enabled, api_port,
			prompt_ctx_size, prompt_memory_token_budget, prompt_conversation_reserve,
			queue_max_depth, queue_wal_path,
			metrics_retention_days,
			saved_at
		FROM config WHERE id = 1`)

	var (
		cfg         config.Config
		openOnStart int
		apiEnabled  int
		savedAt     sql.NullInt64
	)
	err := row.Scan(
		&cfg.Model.Binary, &cfg.Model.ModelPath, &cfg.Model.CtxSize, &cfg.Model.GPULayers,
		&cfg.Model.NParallel, &cfg.Model.Port,
		&cfg.Embedder.Binary, &cfg.Embedder.ModelPath, &cfg.Embedder.Port,
		&cfg.Memory.RepoPath,
		&cfg.UI.Port, &openOnStart,
		&apiEnabled, &cfg.API.Port,
		&cfg.Prompt.CtxSize, &cfg.Prompt.MemoryTokenBudget, &cfg.Prompt.ConversationReserve,
		&cfg.Queue.MaxDepth, &cfg.Queue.WALPath,
		&cfg.Metrics.RetentionDays,
		&savedAt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("db: load config: %w", err)
	}
	cfg.UI.OpenOnStart = openOnStart != 0
	cfg.API.Enabled = apiEnabled != 0
	return &cfg, savedAt.Valid, nil
}

// Save writes cfg and marks the row as user-saved.
func (s *ConfigStore) Save(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("db: save config: nil config")
	}
	_, err := s.db.Exec(`
		UPDATE config SET
			model_binary = ?, model_path = ?, model_ctx_size = ?, model_gpu_layers = ?,
			model_n_parallel = ?, model_port = ?,
			embedder_binary = ?, embedder_model_path = ?, embedder_port = ?,
			memory_repo_path = ?,
			ui_port = ?, ui_open_on_start = ?,
			api_enabled = ?, api_port = ?,
			prompt_ctx_size = ?, prompt_memory_token_budget = ?, prompt_conversation_reserve = ?,
			queue_max_depth = ?, queue_wal_path = ?,
			metrics_retention_days = ?,
			saved_at = ?
		WHERE id = 1`,
		cfg.Model.Binary, cfg.Model.ModelPath, cfg.Model.CtxSize, cfg.Model.GPULayers,
		cfg.Model.NParallel, cfg.Model.Port,
		cfg.Embedder.Binary, cfg.Embedder.ModelPath, cfg.Embedder.Port,
		cfg.Memory.RepoPath,
		cfg.UI.Port, boolInt(cfg.UI.OpenOnStart),
		boolInt(cfg.API.Enabled), cfg.API.Port,
		cfg.Prompt.CtxSize, cfg.Prompt.MemoryTokenBudget, cfg.Prompt.ConversationReserve,
		cfg.Queue.MaxDepth, cfg.Queue.WALPath,
		cfg.Metrics.RetentionDays,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: save config: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
