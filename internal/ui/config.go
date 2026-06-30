package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/vrnc/harness/internal/config"
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
	// llama-server if the user has not entered one yet. We do not pre-fill
	// anything else - datalists let the user pick without us guessing.
	if !skipStoreLoad && data.Config != nil && data.Config.Model.Binary == "" && len(data.Suggestions.LlamaBinary) > 0 {
		data.Config.Model.Binary = data.Suggestions.LlamaBinary[0]
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
	cfg := parseConfigForm(r, &base)

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
// base. Numeric fields that are missing or unparseable keep the base value;
// string fields are always overwritten (an empty required field will surface
// as a validation error downstream).
func parseConfigForm(r *http.Request, base *config.Config) *config.Config {
	cfg := *base

	cfg.Model.Binary = strings.TrimSpace(r.FormValue("model_binary"))
	cfg.Model.ModelPath = strings.TrimSpace(r.FormValue("model_path"))
	cfg.Model.CtxSize = atoiOr(r.FormValue("model_ctx_size"), cfg.Model.CtxSize)
	cfg.Model.GPULayers = atoiOr(r.FormValue("model_gpu_layers"), cfg.Model.GPULayers)
	cfg.Model.NParallel = atoiOr(r.FormValue("model_n_parallel"), cfg.Model.NParallel)
	cfg.Model.Port = atoiOr(r.FormValue("model_port"), cfg.Model.Port)
	cfg.Model.Verbose = r.FormValue("model_verbose") == "on"
	// Cache types are a constrained enum (see config.ValidCacheTypes). Treat
	// missing/blank like numeric fields - keep the base value rather than
	// snapping to "" and tripping Validate. The select always submits its
	// current option, so this only matters for partial form posts and tests.
	if v := strings.TrimSpace(r.FormValue("model_cache_type_k")); v != "" {
		cfg.Model.CacheTypeK = v
	}
	if v := strings.TrimSpace(r.FormValue("model_cache_type_v")); v != "" {
		cfg.Model.CacheTypeV = v
	}

	cfg.Embedder.Binary = strings.TrimSpace(r.FormValue("embed_binary"))
	cfg.Embedder.ModelPath = strings.TrimSpace(r.FormValue("embed_path"))
	cfg.Embedder.Port = atoiOr(r.FormValue("embed_port"), cfg.Embedder.Port)
	cfg.Embedder.Verbose = r.FormValue("embed_verbose") == "on"

	cfg.Memory.RepoPath = strings.TrimSpace(r.FormValue("memory_repo"))

	cfg.UI.Port = atoiOr(r.FormValue("ui_port"), cfg.UI.Port)
	cfg.UI.OpenOnStart = r.FormValue("ui_open_on_start") == "on"

	cfg.API.Enabled = r.FormValue("api_enabled") == "on"
	cfg.API.Port = atoiOr(r.FormValue("api_port"), cfg.API.Port)

	cfg.Prompt.CtxSize = atoiOr(r.FormValue("prompt_ctx_size"), cfg.Prompt.CtxSize)
	cfg.Prompt.MemoryTokenBudget = atoiOr(r.FormValue("prompt_memory_budget"), cfg.Prompt.MemoryTokenBudget)
	cfg.Prompt.ConversationReserve = atoiOr(r.FormValue("prompt_conversation_reserve"), cfg.Prompt.ConversationReserve)
	cfg.Prompt.RecencyN = atoiOr(r.FormValue("prompt_recency_n"), cfg.Prompt.RecencyN)
	cfg.Prompt.PromotionDedupThreshold = atofOr(r.FormValue("prompt_promotion_dedup_threshold"), cfg.Prompt.PromotionDedupThreshold)
	// Trim trailing whitespace so an empty textarea (which browsers may
	// pad with a stray newline) is treated as "use the built-in default"
	// rather than persisting whitespace.
	cfg.Prompt.SummarizerPrompt = strings.TrimSpace(r.FormValue("prompt_summarizer_prompt"))

	cfg.Queue.MaxDepth = atoiOr(r.FormValue("queue_max_depth"), cfg.Queue.MaxDepth)
	cfg.Queue.WALPath = strings.TrimSpace(r.FormValue("queue_wal_path"))

	cfg.Loop.MaxTurns = atoiOr(r.FormValue("loop_max_turns"), cfg.Loop.MaxTurns)
	cfg.Loop.DoomThreshold = atoiOr(r.FormValue("loop_doom_threshold"), cfg.Loop.DoomThreshold)
	cfg.Loop.FileReadEnabled = r.FormValue("loop_file_read_enabled") == "on"
	cfg.Loop.FileListEnabled = r.FormValue("loop_file_list_enabled") == "on"

	cfg.Metrics.RetentionDays = atoiOr(r.FormValue("metrics_retention_days"), cfg.Metrics.RetentionDays)

	cfg.Log.RingMaxEntries = atoiOr(r.FormValue("log_ring_max_entries"), cfg.Log.RingMaxEntries)
	cfg.Log.ProcMaxLines = atoiOr(r.FormValue("log_proc_max_lines"), cfg.Log.ProcMaxLines)

	return &cfg
}

func atoiOr(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func atofOr(s string, fallback float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return n
}
