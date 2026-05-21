package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetect_EmptyBinDirReturnsNothing(t *testing.T) {
	s := Detect("")
	if len(s.LlamaBinary) != 0 || len(s.MainModel) != 0 || len(s.EmbedModel) != 0 {
		t.Errorf("expected empty suggestions for empty binDir, got %+v", s)
	}
}

func TestDetect_NoModelsDir(t *testing.T) {
	s := Detect(t.TempDir())
	if len(s.MainModel) != 0 || len(s.EmbedModel) != 0 {
		t.Errorf("expected no model suggestions when models/ is absent, got %+v", s)
	}
}

func TestDetect_ClassifiesModels(t *testing.T) {
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.Mkdir(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{
		"Qwen3-35B-UD-Q3.gguf",
		"llama-3-8b.gguf",
		"nomic-embed-text-v2.gguf",
		"mxbai-embed-large.gguf",
		"readme.txt",
	} {
		if err := os.WriteFile(filepath.Join(modelsDir, name), nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	s := Detect(dir)

	has := func(list []string, suffix string) bool {
		for _, p := range list {
			if strings.HasSuffix(p, suffix) {
				return true
			}
		}
		return false
	}

	if !has(s.MainModel, "Qwen3-35B-UD-Q3.gguf") || !has(s.MainModel, "llama-3-8b.gguf") {
		t.Errorf("expected main models in MainModel, got %v", s.MainModel)
	}
	if !has(s.EmbedModel, "nomic-embed-text-v2.gguf") || !has(s.EmbedModel, "mxbai-embed-large.gguf") {
		t.Errorf("expected embedder models in EmbedModel, got %v", s.EmbedModel)
	}
	if has(s.MainModel, "nomic-embed-text-v2.gguf") || has(s.EmbedModel, "Qwen3-35B-UD-Q3.gguf") {
		t.Error("embedder/main classification crossed over")
	}
	if has(s.MainModel, "readme.txt") || has(s.EmbedModel, "readme.txt") {
		t.Error("non-gguf file leaked into suggestions")
	}
}

func TestDetect_FindsModelsInParentOfBinDir(t *testing.T) {
	// Dev layout: binary in dist/, models/ at repo root. Detect should find
	// models under <binDir>/../models.
	root := t.TempDir()
	binDir := filepath.Join(root, "dist")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}
	modelsDir := filepath.Join(root, "models")
	if err := os.Mkdir(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir models: %v", err)
	}
	main := filepath.Join(modelsDir, "Qwen3-35B.gguf")
	embed := filepath.Join(modelsDir, "nomic-embed.gguf")
	for _, p := range []string{main, embed} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	s := Detect(binDir)
	if len(s.MainModel) != 1 || s.MainModel[0] != main {
		t.Errorf("expected MainModel=[%s], got %v", main, s.MainModel)
	}
	if len(s.EmbedModel) != 1 || s.EmbedModel[0] != embed {
		t.Errorf("expected EmbedModel=[%s], got %v", embed, s.EmbedModel)
	}
}

func TestDetect_DedupesModelsAcrossRoots(t *testing.T) {
	// If both binDir and parent contain models/ with the same file, Detect
	// must not return it twice.
	root := t.TempDir()
	binDir := filepath.Join(root, "dist")
	for _, d := range []string{binDir, filepath.Join(root, "models"), filepath.Join(binDir, "models")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	rootModel := filepath.Join(root, "models", "same.gguf")
	distModel := filepath.Join(binDir, "models", "dist-only.gguf")
	for _, p := range []string{rootModel, distModel} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	s := Detect(binDir)
	if len(s.MainModel) != 2 {
		t.Errorf("expected 2 main models (one per root, no dupes), got %v", s.MainModel)
	}
}

func TestDetect_FindsLlamaBinaryNextToBinary(t *testing.T) {
	dir := t.TempDir()
	exe := "llama-server"
	nested := filepath.Join(dir, "llama.cpp")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(nested, exe)
	if err := os.WriteFile(want, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := Detect(dir)
	found := false
	for _, p := range s.LlamaBinary {
		if p == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %s in LlamaBinary, got %v", want, s.LlamaBinary)
	}
}

func TestDetect_DedupAbs(t *testing.T) {
	in := []string{"a", "b", "a", "./a"}
	out := dedupAbs(in)
	if len(out) != 2 {
		t.Errorf("expected 2 unique paths, got %d (%v)", len(out), out)
	}
}

func TestLooksLikeEmbedder(t *testing.T) {
	cases := map[string]bool{
		"nomic-embed-text-v2.gguf": true,
		"mxbai-embed-large.gguf":   true,
		"BGE-EMBEDDINGS.gguf":      true,
		"Qwen3-35B-UD-Q3.gguf":     false,
		"llama-3-8b-instruct.gguf": false,
		"mistral-7b-embedly.gguf":  true, // intentional: substring match is enough
	}
	for name, want := range cases {
		if got := looksLikeEmbedder(name); got != want {
			t.Errorf("looksLikeEmbedder(%q) = %v, want %v", name, got, want)
		}
	}
}
