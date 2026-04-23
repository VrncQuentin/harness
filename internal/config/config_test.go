package config

import "testing"

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.UI.Port != 3000 {
		t.Errorf("expected default UI port 3000, got %d", d.UI.Port)
	}
	if d.Queue.MaxDepth != 8 {
		t.Errorf("expected default queue depth 8, got %d", d.Queue.MaxDepth)
	}
	if d.Metrics.RetentionDays != 30 {
		t.Errorf("expected default retention 30, got %d", d.Metrics.RetentionDays)
	}
	if !d.UI.OpenOnStart {
		t.Error("expected default UI.OpenOnStart=true")
	}
}

func TestValidate(t *testing.T) {
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
