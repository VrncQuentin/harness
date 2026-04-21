package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for missing config.toml, got nil")
	}
}

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	content := `
[model]
binary = "/path/to/llama-server"
model_path = "/path/to/model.gguf"
ctx_size = 4096
gpu_layers = 10
n_parallel = 1

[embedder]
binary = "/path/to/embedder"
model_path = "/path/to/embed.gguf"

[ui]
port = 3000
open_on_start = false
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model.Binary != "/path/to/llama-server" {
		t.Errorf("unexpected binary: %s", cfg.Model.Binary)
	}
	if cfg.Model.CtxSize != 4096 {
		t.Errorf("unexpected ctx_size: %d", cfg.Model.CtxSize)
	}
}

func TestLoadInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not valid toml [[["), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for invalid TOML, got nil")
	}
}

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.UI.Port != 3000 {
		t.Errorf("expected default port 3000, got %d", d.UI.Port)
	}
	if d.Queue.MaxDepth != 8 {
		t.Errorf("expected default queue depth 8, got %d", d.Queue.MaxDepth)
	}
}
