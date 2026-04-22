// Package config loads, validates, and persists the harness configuration.
// Configuration lives in the shared harness SQLite database as a single-row
// typed table. There is no on-disk config file.
package config

import (
	"database/sql"
	"fmt"
	"time"
)

// Config is the top-level configuration structure for the harness.
type Config struct {
	Model    ModelConfig
	Embedder EmbedderConfig
	Memory   MemoryConfig
	UI       UIConfig
	API      APIConfig
	Prompt   PromptConfig
	Queue    QueueConfig
	Metrics  MetricsConfig
}

// ModelConfig holds llama-server configuration.
type ModelConfig struct {
	Binary    string
	ModelPath string
	CtxSize   int
	GPULayers int
	NParallel int
	Port      int
}

// EmbedderConfig holds the embedder sidecar configuration.
type EmbedderConfig struct {
	Binary    string
	ModelPath string
	Port      int
}

// MemoryConfig holds memory repo configuration.
type MemoryConfig struct {
	RepoPath string
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
}

// QueueConfig holds queue configuration.
type QueueConfig struct {
	MaxDepth int
	WALPath  string
}

// MetricsConfig holds metrics retention configuration. The database file
// itself is the shared harness.db next to the binary, not configurable here.
type MetricsConfig struct {
	RetentionDays int
}

// Defaults returns a Config with sensible defaults applied.
func Defaults() Config {
	return Config{
		Model: ModelConfig{
			CtxSize:   32768,
			GPULayers: 35,
			NParallel: 1,
			Port:      8081,
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
			CtxSize:             32768,
			MemoryTokenBudget:   6144,
			ConversationReserve: 8192,
		},
		Queue: QueueConfig{
			MaxDepth: 8,
		},
		Metrics: MetricsConfig{
			RetentionDays: 30,
		},
	}
}

// Store persists Config in a SQLite database. The same *sql.DB is shared with
// other harness subsystems (e.g. metrics) - Store does not own or close it.
type Store struct {
	db *sql.DB
}

// Open runs the config migration and seeds the defaults row if missing. The
// caller owns db and is responsible for closing it.
func Open(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("config: nil db handle")
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	if err := seed(db); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Load returns the current config and whether the user has explicitly saved
// it at least once. A fresh install returns (Defaults(), false, nil).
func (s *Store) Load() (*Config, bool, error) {
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
		cfg            Config
		openOnStart    int
		apiEnabled     int
		savedAt        sql.NullInt64
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
		return nil, false, fmt.Errorf("config: load: %w", err)
	}
	cfg.UI.OpenOnStart = openOnStart != 0
	cfg.API.Enabled = apiEnabled != 0
	return &cfg, savedAt.Valid, nil
}

// Save writes cfg and marks the row as user-saved.
func (s *Store) Save(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: save: nil config")
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
		return fmt.Errorf("config: save: %w", err)
	}
	return nil
}

// Validate checks that required fields are present.
func Validate(cfg *Config) error {
	if cfg.Model.Binary == "" {
		return fmt.Errorf("config: model.binary is required")
	}
	if cfg.Model.ModelPath == "" {
		return fmt.Errorf("config: model.model_path is required")
	}
	if cfg.Model.Port == 0 {
		return fmt.Errorf("config: model.port must be non-zero")
	}
	if cfg.Embedder.Binary == "" {
		return fmt.Errorf("config: embedder.binary is required")
	}
	if cfg.Embedder.ModelPath == "" {
		return fmt.Errorf("config: embedder.model_path is required")
	}
	if cfg.Embedder.Port == 0 {
		return fmt.Errorf("config: embedder.port must be non-zero")
	}
	if cfg.UI.Port == 0 {
		return fmt.Errorf("config: ui.port must be non-zero")
	}
	return nil
}

func migrate(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS config (
	id                           INTEGER PRIMARY KEY CHECK (id = 1),
	model_binary                 TEXT    NOT NULL DEFAULT '',
	model_path                   TEXT    NOT NULL DEFAULT '',
	model_ctx_size               INTEGER NOT NULL DEFAULT 32768,
	model_gpu_layers             INTEGER NOT NULL DEFAULT 35,
	model_n_parallel             INTEGER NOT NULL DEFAULT 1,
	model_port                   INTEGER NOT NULL DEFAULT 8081,
	embedder_binary              TEXT    NOT NULL DEFAULT '',
	embedder_model_path          TEXT    NOT NULL DEFAULT '',
	embedder_port                INTEGER NOT NULL DEFAULT 8082,
	memory_repo_path             TEXT    NOT NULL DEFAULT '',
	ui_port                      INTEGER NOT NULL DEFAULT 3000,
	ui_open_on_start             INTEGER NOT NULL DEFAULT 1,
	api_enabled                  INTEGER NOT NULL DEFAULT 0,
	api_port                     INTEGER NOT NULL DEFAULT 8080,
	prompt_ctx_size              INTEGER NOT NULL DEFAULT 32768,
	prompt_memory_token_budget   INTEGER NOT NULL DEFAULT 6144,
	prompt_conversation_reserve  INTEGER NOT NULL DEFAULT 8192,
	queue_max_depth              INTEGER NOT NULL DEFAULT 8,
	queue_wal_path               TEXT    NOT NULL DEFAULT '',
	metrics_retention_days       INTEGER NOT NULL DEFAULT 30,
	saved_at                     INTEGER
);`
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("config: migrate: %w", err)
	}
	return nil
}

// seed inserts the singleton row if it doesn't exist. Column defaults supply
// the initial values, so Defaults() and the DDL defaults must stay in sync.
func seed(db *sql.DB) error {
	if _, err := db.Exec(`INSERT OR IGNORE INTO config (id) VALUES (1)`); err != nil {
		return fmt.Errorf("config: seed: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
