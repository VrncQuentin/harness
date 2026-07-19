package runtime

import (
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
	return proc.LlamaArgs(proc.LlamaArgsConfig{
		Binary:     model.Binary,
		ModelPath:  model.ModelPath,
		CtxSize:    model.CtxSize,
		GPULayers:  model.GPULayers,
		NParallel:  model.NParallel,
		Port:       model.Port,
		Verbose:    model.Verbose,
		CacheTypeK: model.CacheTypeK,
		CacheTypeV: model.CacheTypeV,
	})
}

func llamaHealthURL(model config.ModelConfig) string {
	return proc.HealthURL(model.Port)
}

func embedderArgsForConfig(embed config.EmbedderConfig) (string, []string) {
	return proc.EmbedderArgs(proc.EmbedderArgsConfig{
		Binary:    embed.Binary,
		ModelPath: embed.ModelPath,
		Port:      embed.Port,
		Verbose:   embed.Verbose,
	})
}

func embedderHealthURL(embed config.EmbedderConfig) string {
	return proc.HealthURL(embed.Port)
}
