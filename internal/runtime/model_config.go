package runtime

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/VrncQuentin/harness/internal/config"
	"github.com/VrncQuentin/harness/internal/project"
)

func (rt *Runtime) effectiveModelFor(cfg *config.Config) config.ModelConfig {
	if rt.projectStore == nil {
		return cfg.Model
	}
	slug := cfg.Project.ActiveProjectSlug
	if slug == "" {
		slug = project.GlobalSlug
	}
	proj, err := rt.projectStore.Get(slug)
	if err != nil {
		slog.Warn("runtime: using global model config; active project model overrides unavailable", "slug", slug, "err", err)
		return cfg.Model
	}
	return config.EffectiveModel(cfg, &proj)
}

// promptConfigFor derives a generation's prompt config: the persisted prompt
// fields plus the context-size ceiling of the model the generation will
// actually talk to — the running model, which under llama_on_switch=keep may
// lag the active project's preferred model.
func promptConfigFor(cfg *config.Config, runningModel config.ModelConfig) config.PromptConfig {
	promptCfg := cfg.Prompt
	promptCfg.CtxSize = runningModel.CtxSize
	return promptCfg
}

func llamaArgsForModel(model config.ModelConfig) (string, []string) {
	args := []string{
		"--model", model.ModelPath,
		"--ctx-size", strconv.Itoa(model.CtxSize),
		"--n-gpu-layers", strconv.Itoa(model.GPULayers),
		"--parallel", strconv.Itoa(model.NParallel),
		"--port", strconv.Itoa(model.Port),
		"--host", "127.0.0.1",
		"--cache-type-k", model.CacheTypeK,
		"--cache-type-v", model.CacheTypeV,
	}
	if model.Verbose {
		args = append(args, "--verbose")
	}
	return model.Binary, args
}

func llamaHealthURL(model config.ModelConfig) string {
	return localHealthURL(model.Port)
}

func embedderArgsForConfig(embed config.EmbedderConfig) (string, []string) {
	args := []string{
		"--model", embed.ModelPath,
		"--embedding",
		"--n-gpu-layers", "0",
		"--port", strconv.Itoa(embed.Port),
		"--host", "127.0.0.1",
	}
	if embed.Verbose {
		args = append(args, "--verbose")
	}
	return embed.Binary, args
}

func embedderHealthURL(embed config.EmbedderConfig) string {
	return localHealthURL(embed.Port)
}

func localHealthURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/health", port)
}
