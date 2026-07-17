package runtime

import (
	"fmt"
	"log/slog"

	"github.com/vrnc/harness/internal/config"
	"github.com/vrnc/harness/internal/proc"
)

func (rt *Runtime) effectiveModelFor(cfg *config.Config) config.ModelConfig {
	if rt.projectStore == nil {
		return cfg.Model
	}
	proj, err := rt.resolveProject(cfg.Project.ActiveProjectSlug)
	if err != nil {
		slog.Warn("runtime: using global model config; active project model overrides unavailable", "slug", cfg.Project.ActiveProjectSlug, "err", err)
		return cfg.Model
	}
	return config.EffectiveModel(cfg, proj)
}

func (rt *Runtime) effectivePromptFor(cfg *config.Config) config.PromptConfig {
	promptCfg := cfg.Prompt
	promptCfg.CtxSize = rt.effectiveModelFor(cfg).CtxSize
	return promptCfg
}
func llamaArgsForModel(model config.ModelConfig) (string, []string) {
	return proc.LlamaArgs(
		model.Binary,
		model.ModelPath,
		model.CtxSize,
		model.GPULayers,
		model.NParallel,
		model.Port,
		model.Verbose,
		model.CacheTypeK,
		model.CacheTypeV,
	)
}

func llamaHealthURL(model config.ModelConfig) string {
	return fmt.Sprintf("http://127.0.0.1:%d/health", model.Port)
}
