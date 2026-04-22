// Package config defines the harness configuration schema, defaults, and
// validation. Persistence lives in internal/db - this package holds no SQL.
package config

import "fmt"

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

// Store persists and retrieves Config. The concrete implementation lives in
// internal/db; callers accept this interface so they can be tested with
// in-memory fakes.
type Store interface {
	Load() (*Config, bool, error)
	Save(*Config) error
}

// Defaults returns a Config with sensible defaults applied. The column
// defaults in the 0001_init migration must stay in sync with these values.
func Defaults() Config {
	return Config{
		Model: ModelConfig{
			CtxSize:   32768,
			GPULayers: -1,
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
