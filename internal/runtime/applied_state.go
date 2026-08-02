package runtime

import (
	"github.com/VrncQuentin/harness/internal/config"
)

// appliedState is the runtime's single recorded record of the facts about the
// live system that config applies and project switches compare against,
// publish, and roll back to. It is installed as one coherent object together
// with the generation under rt.mu; the runtime never reconstructs "the old
// state" from the mutable config store or the mutable project store.
//
// model and runningModel are deliberately separate: model is the preferred
// effective model for the active project (global model config overlaid with
// the active project's overrides), while runningModel is the model llama-server
// is actually configured to run. They differ when project.llama_on_switch=keep
// leaves the previous model running across a config apply or project switch;
// the status UI renders that mismatch honestly from these two values.
type appliedState struct {
	// cfg is the config committed with this state.
	cfg config.Config

	// activeSlug is the active project the committed generation serves.
	activeSlug string

	// model is the preferred/effective model for the active project.
	model config.ModelConfig

	// embedder is the effective embedder config for this state.
	embedder config.EmbedderConfig

	// runningModel is the model llama-server is actually configured to run.
	// It may differ from model when llama_on_switch=keep is in effect.
	runningModel config.ModelConfig

	// runningEmbedder is the embedder config the sidecar actually runs.
	runningEmbedder config.EmbedderConfig
}

// newAppliedState builds the applied state for a config apply. runningModel
// must already reflect the llama_on_switch decision made by the caller.
func newAppliedState(cfg *config.Config, preferred, running config.ModelConfig) appliedState {
	return appliedState{
		cfg:             *cfg,
		activeSlug:      cfg.Project.ActiveProjectSlug,
		model:           preferred,
		embedder:        cfg.Embedder,
		runningModel:    running,
		runningEmbedder: cfg.Embedder,
	}
}
