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

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.Model.Binary = "/path/to/llama-server"
	cfg.Model.ModelPath = "/path/to/model.gguf"
	cfg.Embedder.Binary = "/path/to/embedder"
	cfg.Embedder.ModelPath = "/path/to/embed.gguf"

	if err := Save(&cfg, dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Model.Binary != cfg.Model.Binary {
		t.Errorf("binary roundtrip: got %q, want %q", loaded.Model.Binary, cfg.Model.Binary)
	}
	if loaded.UI.Port != cfg.UI.Port {
		t.Errorf("ui port roundtrip: got %d, want %d", loaded.UI.Port, cfg.UI.Port)
	}
	if loaded.Queue.MaxDepth != cfg.Queue.MaxDepth {
		t.Errorf("queue max_depth roundtrip: got %d, want %d", loaded.Queue.MaxDepth, cfg.Queue.MaxDepth)
	}
}

func TestSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.Model.Binary = "/a"
	cfg.Model.ModelPath = "/a.gguf"
	cfg.Embedder.Binary = "/b"
	cfg.Embedder.ModelPath = "/b.gguf"

	if err := Save(&cfg, dir); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// Overwrite with different content; no temp file should remain.
	cfg.Model.Binary = "/c"
	if err := Save(&cfg, dir); err != nil {
		t.Fatalf("second save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("unexpected leftover temp file: %s", e.Name())
		}
	}
}

func TestValidateExposed(t *testing.T) {
	cfg := Defaults()
	if err := Validate(&cfg); err == nil {
		t.Error("expected Validate to fail on empty defaults (missing required paths)")
	}
	cfg.Model.Binary = "/x"
	cfg.Model.ModelPath = "/y.gguf"
	cfg.Embedder.Binary = "/a"
	cfg.Embedder.ModelPath = "/b.gguf"
	if err := Validate(&cfg); err != nil {
		t.Errorf("expected Validate to pass once required fields set: %v", err)
	}
}
