package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/VrncQuentin/harness/internal/config"
)

// configPageData is the template context for the config editor.
type configPageData struct {
	basePage
	Config         *config.Config
	Suggestions    config.Suggestions
	CacheTypes     []string
	FirstRun       bool
	Saved          bool
	LiveApplied    bool
	RestartReasons []string
	ValidationErr  string
	SaveErr        string
}

// handleConfig serves GET (render form) and POST (save + re-validate).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderConfig(w, r, configPageData{}, false /* skipStoreLoad */)
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// renderConfig renders /config with the given overlay data (error/success flags).
// If cfg in overlay is nil, it is populated from the store.
func (s *Server) renderConfig(w http.ResponseWriter, r *http.Request, overlay configPageData, skipStoreLoad bool) {
	data := overlay
	data.basePage = s.newBasePage("config")

	if data.Config == nil && !skipStoreLoad {
		store := s.configStore()
		if store == nil {
			data.SaveErr = "config store unavailable (harness.db could not be opened)"
			d := config.Defaults()
			data.Config = &d
			data.FirstRun = true
		} else {
			cfg, configured, err := store.Load()
			if err != nil {
				data.SaveErr = err.Error()
				d := config.Defaults()
				data.Config = &d
				data.FirstRun = true
			} else {
				data.Config = cfg
				data.FirstRun = !configured
			}
		}
	}
	if r.URL.Query().Get("saved") == "1" {
		data.Saved = true
		data.LiveApplied = r.URL.Query().Get("applied") == "1"
		if rs := r.URL.Query().Get("restart"); rs != "" {
			data.RestartReasons = strings.Split(rs, "|")
		}
	}

	data.Suggestions = config.Detect(s.getBinDir())
	data.CacheTypes = config.ValidCacheTypes
	// On a fresh GET render, pre-fill model_binary with the first detected
	// llama-server if the user has not entered one yet. The embedder runs
	// the same binary in --embedding mode, so it defaults to the same
	// resolved path. We do not pre-fill anything else - datalists let the
	// user pick without us guessing.
	if !skipStoreLoad && data.Config != nil && len(data.Suggestions.LlamaBinary) > 0 {
		if data.Config.Model.Binary == "" {
			data.Config.Model.Binary = data.Suggestions.LlamaBinary[0]
		}
		if data.Config.Embedder.Binary == "" {
			data.Config.Embedder.Binary = data.Suggestions.LlamaBinary[0]
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.configTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// saveConfig parses the form, validates, writes, then triggers retry.
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderConfig(w, r, configPageData{SaveErr: "could not parse form: " + err.Error()}, true /* skipStoreLoad */)
		return
	}

	store := s.configStore()
	if store == nil {
		s.renderConfig(w, r, configPageData{SaveErr: "config store unavailable"}, true /* skipStoreLoad */)
		return
	}

	// Use the current saved config as the base so fields the form doesn't
	// touch (or numeric fields left blank) preserve their existing values
	// rather than snapping back to Defaults.
	base := config.Defaults()
	if cur, _, err := store.Load(); err == nil {
		base = *cur
	}
	cfg, parseErrs := parseConfigForm(r, &base)
	if len(parseErrs) > 0 {
		s.renderConfig(w, r, configPageData{Config: cfg, ValidationErr: strings.Join(parseErrs, "; ")}, true /* skipStoreLoad */)
		return
	}

	if err := config.Validate(cfg); err != nil {
		s.renderConfig(w, r, configPageData{Config: cfg, ValidationErr: err.Error()}, true /* skipStoreLoad */)
		return
	}
	if err := store.Save(cfg); err != nil {
		s.renderConfig(w, r, configPageData{Config: cfg, SaveErr: err.Error()}, true /* skipStoreLoad */)
		return
	}

	// Trigger retry so startup errors get cleared/refreshed against the new config.
	result := s.callRetry()
	target := "/config?saved=1"
	if result.LiveApplied {
		target += "&applied=1"
	}
	if len(result.RestartNeeded) > 0 {
		target += "&restart=" + url.QueryEscape(strings.Join(result.RestartNeeded, "|"))
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseConfigForm builds a Config from the posted form, overlaying values on
// base. Blank numeric fields preserve their existing values; malformed numeric
// fields are reported so the form never silently ignores bad input.
func parseConfigForm(r *http.Request, base *config.Config) (*config.Config, []string) {
	cfg := *base
	var parseErrs []string

	cfg.Model.Binary = trimPathField(r.FormValue("model_binary"))
	cfg.Model.ModelPath = trimPathField(r.FormValue("model_path"))
	cfg.Model.CtxSize = atoiField(r, "model_ctx_size", "Model context size", cfg.Model.CtxSize, &parseErrs)
	cfg.Model.GPULayers = atoiField(r, "model_gpu_layers", "Model GPU layers", cfg.Model.GPULayers, &parseErrs)
	cfg.Model.NParallel = atoiField(r, "model_n_parallel", "Model parallelism", cfg.Model.NParallel, &parseErrs)
	cfg.Model.Port = atoiField(r, "model_port", "Model port", cfg.Model.Port, &parseErrs)
	cfg.Model.Verbose = r.FormValue("model_verbose") == "on"
	if v := strings.TrimSpace(r.FormValue("model_cache_type_k")); v != "" {
		cfg.Model.CacheTypeK = v
	}
	if v := strings.TrimSpace(r.FormValue("model_cache_type_v")); v != "" {
		cfg.Model.CacheTypeV = v
	}

	cfg.Embedder.Binary = trimPathField(r.FormValue("embed_binary"))
	cfg.Embedder.ModelPath = trimPathField(r.FormValue("embed_path"))
	cfg.Embedder.Port = atoiField(r, "embed_port", "Embedder port", cfg.Embedder.Port, &parseErrs)
	cfg.Embedder.Verbose = r.FormValue("embed_verbose") == "on"
	cfg.UI.Port = atoiField(r, "ui_port", "UI port", cfg.UI.Port, &parseErrs)
	cfg.UI.OpenOnStart = r.FormValue("ui_open_on_start") == "on"

	cfg.API.Enabled = r.FormValue("api_enabled") == "on"
	cfg.API.Port = atoiField(r, "api_port", "API port", cfg.API.Port, &parseErrs)

	if v := strings.TrimSpace(r.FormValue("project_llama_on_switch")); v != "" {
		cfg.Project.LlamaOnSwitch = v
	}
	cfg.Prompt.MemoryTokenBudget = atoiField(r, "prompt_memory_budget", "Prompt memory token budget", cfg.Prompt.MemoryTokenBudget, &parseErrs)
	cfg.Prompt.ConversationReserve = atoiField(r, "prompt_conversation_reserve", "Prompt conversation reserve", cfg.Prompt.ConversationReserve, &parseErrs)
	cfg.Prompt.RecencyN = atoiField(r, "prompt_recency_n", "Prompt recency N", cfg.Prompt.RecencyN, &parseErrs)
	cfg.Prompt.SemanticWeight = atofField(r, "prompt_semantic_weight", "Prompt semantic weight", cfg.Prompt.SemanticWeight, &parseErrs)
	cfg.Prompt.RecencyWeight = atofField(r, "prompt_recency_weight", "Prompt recency weight", cfg.Prompt.RecencyWeight, &parseErrs)
	cfg.Prompt.PromotionDedupThreshold = atofField(r, "prompt_promotion_dedup_threshold", "Promotion dedup threshold", cfg.Prompt.PromotionDedupThreshold, &parseErrs)
	cfg.Prompt.SummarizerPrompt = strings.TrimSpace(r.FormValue("prompt_summarizer_prompt"))

	cfg.Queue.MaxDepth = atoiField(r, "queue_max_depth", "Queue max depth", cfg.Queue.MaxDepth, &parseErrs)

	cfg.Loop.MaxTurns = atoiField(r, "loop_max_turns", "Agent loop max turns", cfg.Loop.MaxTurns, &parseErrs)
	cfg.Loop.DoomThreshold = atoiField(r, "loop_doom_threshold", "Agent loop doom threshold", cfg.Loop.DoomThreshold, &parseErrs)
	cfg.Loop.ReadEnabled = r.FormValue("loop_read_enabled") == "on"
	cfg.Loop.FileListEnabled = r.FormValue("loop_file_list_enabled") == "on"
	cfg.Loop.AstMapEnabled = r.FormValue("loop_ast_map_enabled") == "on"
	cfg.Loop.AstFindEnabled = r.FormValue("loop_ast_find_enabled") == "on"
	cfg.Loop.GitStatusEnabled = r.FormValue("loop_git_status_enabled") == "on"
	cfg.Loop.GitDiffEnabled = r.FormValue("loop_git_diff_enabled") == "on"
	cfg.Loop.GitLogEnabled = r.FormValue("loop_git_log_enabled") == "on"
	cfg.Loop.EditEnabled = r.FormValue("loop_edit_enabled") == "on"
	cfg.Loop.ExecEnabled = r.FormValue("loop_exec_enabled") == "on"
	cfg.Loop.GoTestEnabled = r.FormValue("loop_go_test_enabled") == "on"
	cfg.Loop.GoLintEnabled = r.FormValue("loop_go_lint_enabled") == "on"
	cfg.Loop.GitCommitEnabled = r.FormValue("loop_git_commit_enabled") == "on"
	cfg.Loop.GitBranchEnabled = r.FormValue("loop_git_branch_enabled") == "on"
	cfg.Loop.GitCheckoutEnabled = r.FormValue("loop_git_checkout_enabled") == "on"
	cfg.Loop.WebSearchEnabled = r.FormValue("loop_web_search_enabled") == "on"
	cfg.Loop.MemoryQueryEnabled = r.FormValue("loop_memory_query_enabled") == "on"
	cfg.Loop.GitPushEnabled = r.FormValue("loop_git_push_enabled") == "on"
	cfg.Loop.GHPRCreateEnabled = r.FormValue("loop_gh_pr_create_enabled") == "on"
	cfg.Loop.GHPRMergeEnabled = r.FormValue("loop_gh_pr_merge_enabled") == "on"
	cfg.Loop.GHPRWaitEnabled = r.FormValue("loop_gh_pr_wait_enabled") == "on"

	cfg.Metrics.RetentionDays = atoiField(r, "metrics_retention_days", "Metrics retention days", cfg.Metrics.RetentionDays, &parseErrs)
	cfg.Metrics.PrometheusEnabled = r.FormValue("metrics_prometheus_enabled") == "on"

	cfg.Log.RingMaxEntries = atoiField(r, "log_ring_max_entries", "Harness log entries", cfg.Log.RingMaxEntries, &parseErrs)
	cfg.Log.ProcMaxLines = atoiField(r, "log_proc_max_lines", "Process log lines", cfg.Log.ProcMaxLines, &parseErrs)

	return &cfg, parseErrs
}

// trimPathField normalizes a filesystem path pasted into the form. Windows'
// "Copy as path" action wraps the path in double quotes and users often paste
// that verbatim; strip surrounding matching quotes (and surrounding
// whitespace) so the stored value is the raw path. A quote embedded mid-path
// is never valid on Windows and a leading/trailing quote is never part of a
// real path, so stripping is always safe.
func trimPathField(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '"' || v[0] == '\'') {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	return v
}

func atoiField(r *http.Request, name, label string, fallback int, errs *[]string) int {
	s := strings.TrimSpace(r.FormValue(name))
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		*errs = append(*errs, label+" must be an integer")
		return fallback
	}
	return n
}

func atofField(r *http.Request, name, label string, fallback float64, errs *[]string) float64 {
	s := strings.TrimSpace(r.FormValue(name))
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*errs = append(*errs, label+" must be a number")
		return fallback
	}
	return n
}
