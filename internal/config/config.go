// Package config loads and validates the harness configuration from config.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration structure for the harness.
type Config struct {
	Model   ModelConfig   `toml:"model"`
	Embedder EmbedderConfig `toml:"embedder"`
	Memory  MemoryConfig  `toml:"memory"`
	UI      UIConfig      `toml:"ui"`
	API     APIConfig     `toml:"api"`
	Prompt  PromptConfig  `toml:"prompt"`
	Queue   QueueConfig   `toml:"queue"`
	Metrics MetricsConfig `toml:"metrics"`
}

// ModelConfig holds llama-server configuration.
type ModelConfig struct {
	Binary    string `toml:"binary"`
	ModelPath string `toml:"model_path"`
	CtxSize   int    `toml:"ctx_size"`
	GPULayers int    `toml:"gpu_layers"`
	NParallel int    `toml:"n_parallel"`
	Port      int    `toml:"port"` // default: 8081
}

// EmbedderConfig holds the embedder sidecar configuration.
type EmbedderConfig struct {
	Binary    string `toml:"binary"`
	ModelPath string `toml:"model_path"`
	Port      int    `toml:"port"` // default: 8082
}

// MemoryConfig holds memory repo configuration.
type MemoryConfig struct {
	RepoPath string `toml:"repo_path"`
}

// UIConfig holds UI server configuration.
type UIConfig struct {
	Port        int  `toml:"port"`
	OpenOnStart bool `toml:"open_on_start"`
}

// APIConfig holds the optional API server configuration.
type APIConfig struct {
	Enabled bool `toml:"enabled"`
	Port    int  `toml:"port"`
}

// PromptConfig holds prompt assembly configuration.
type PromptConfig struct {
	CtxSize             int `toml:"ctx_size"`
	MemoryTokenBudget   int `toml:"memory_token_budget"`
	ConversationReserve int `toml:"conversation_reserve"`
}

// QueueConfig holds queue configuration.
type QueueConfig struct {
	MaxDepth int    `toml:"max_depth"`
	WALPath  string `toml:"wal_path"`
}

// MetricsConfig holds metrics storage configuration.
type MetricsConfig struct {
	DBPath        string `toml:"db_path"`
	RetentionDays int    `toml:"retention_days"`
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
			DBPath:        "metrics.db",
			RetentionDays: 30,
		},
	}
}

// Load reads config.toml from dir and returns the parsed Config.
// Returns a non-nil error if the file is missing or malformed.
func Load(dir string) (*Config, error) {
	path := filepath.Join(dir, "config.toml")

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("config: config.toml not found at %s", path)
	}

	cfg := Defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: failed to parse config.toml: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate checks that required fields are present.
func validate(cfg *Config) error {
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

// ConfigPath returns the expected path to config.toml given the binary directory.
func ConfigPath(dir string) string {
	return filepath.Join(dir, "config.toml")
}
