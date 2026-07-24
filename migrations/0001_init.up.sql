CREATE TABLE IF NOT EXISTS projects (
    slug             TEXT PRIMARY KEY,
    display_name     TEXT NOT NULL,
    memory_repo_path TEXT,
    model_binary     TEXT,
    model_path       TEXT,
    model_ctx_size   INTEGER,
    model_gpu_layers INTEGER,
    model_n_parallel INTEGER,
    hidden           INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    saved_at         INTEGER
);

CREATE TABLE IF NOT EXISTS project_directories (
    project_slug TEXT NOT NULL,
    path         TEXT NOT NULL,
    PRIMARY KEY (project_slug, path),
    FOREIGN KEY (project_slug) REFERENCES projects(slug) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS config (
    id                               INTEGER PRIMARY KEY CHECK (id = 1),
    model_binary                     TEXT    NOT NULL,
    model_path                       TEXT    NOT NULL,
    model_ctx_size                   INTEGER NOT NULL,
    model_gpu_layers                 INTEGER NOT NULL,
    model_n_parallel                 INTEGER NOT NULL,
    model_port                       INTEGER NOT NULL,
    model_verbose                    INTEGER NOT NULL,
    model_cache_type_k               TEXT    NOT NULL,
    model_cache_type_v               TEXT    NOT NULL,
    embedder_binary                  TEXT    NOT NULL,
    embedder_model_path              TEXT    NOT NULL,
    embedder_port                    INTEGER NOT NULL,
    embedder_verbose                 INTEGER NOT NULL,
    agent_active                     TEXT    NOT NULL,
    ui_port                          INTEGER NOT NULL,
    ui_open_on_start                 INTEGER NOT NULL,
    api_enabled                      INTEGER NOT NULL,
    api_port                         INTEGER NOT NULL,
    prompt_memory_token_budget       INTEGER NOT NULL,
    prompt_conversation_reserve      INTEGER NOT NULL,
    prompt_recency_n                 INTEGER NOT NULL,
    prompt_summarizer_prompt         TEXT    NOT NULL,
    prompt_semantic_weight           REAL    NOT NULL,
    prompt_recency_weight            REAL    NOT NULL,
    prompt_promotion_dedup_threshold REAL    NOT NULL,
    queue_max_depth                  INTEGER NOT NULL,
    metrics_retention_days           INTEGER NOT NULL,
    metrics_prometheus_enabled       INTEGER NOT NULL,
    log_ring_max_entries             INTEGER NOT NULL,
    log_proc_max_lines               INTEGER NOT NULL,
    active_project_slug              TEXT    NOT NULL,
    project_llama_on_switch          TEXT    NOT NULL,
    loop_max_turns                   INTEGER NOT NULL,
    loop_doom_threshold              INTEGER NOT NULL,
    loop_read_enabled                INTEGER NOT NULL,
    loop_file_list_enabled           INTEGER NOT NULL,
    loop_ast_map_enabled             INTEGER NOT NULL,
    loop_ast_find_enabled            INTEGER NOT NULL,
    loop_git_status_enabled          INTEGER NOT NULL,
    loop_git_diff_enabled            INTEGER NOT NULL,
    loop_git_log_enabled             INTEGER NOT NULL,
    loop_edit_enabled                INTEGER NOT NULL,
    loop_shell_exec_enabled          INTEGER NOT NULL,
    loop_web_search_enabled          INTEGER NOT NULL,
    saved_at                         INTEGER
);

CREATE TABLE IF NOT EXISTS metrics (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    name  TEXT    NOT NULL,
    value REAL    NOT NULL,
    tags  TEXT,
    ts    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS metrics_name_ts ON metrics(name, ts);

CREATE TABLE IF NOT EXISTS metrics_hourly (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    tags       TEXT    NOT NULL DEFAULT '',
    hour_ts    INTEGER NOT NULL,
    count      INTEGER NOT NULL,
    min_value  REAL    NOT NULL,
    max_value  REAL    NOT NULL,
    avg_value  REAL    NOT NULL,
    last_value REAL    NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(name, tags, hour_ts)
);

CREATE INDEX IF NOT EXISTS metrics_hourly_name_hour ON metrics_hourly(name, hour_ts);

CREATE TRIGGER IF NOT EXISTS trg_protect_global_project_delete
BEFORE DELETE ON projects
FOR EACH ROW
WHEN OLD.slug = 'global'
BEGIN
    SELECT RAISE(ABORT, 'cannot delete the global project');
END;

CREATE TRIGGER IF NOT EXISTS trg_protect_global_project_slug_update
BEFORE UPDATE OF slug ON projects
FOR EACH ROW
WHEN OLD.slug = 'global'
BEGIN
    SELECT RAISE(ABORT, 'cannot rename the global project slug');
END;

CREATE TRIGGER IF NOT EXISTS trg_protect_global_project_hide
BEFORE UPDATE OF hidden ON projects
FOR EACH ROW
WHEN OLD.slug = 'global' AND NEW.hidden != 0
BEGIN
    SELECT RAISE(ABORT, 'cannot hide the global project');
END;

CREATE TRIGGER IF NOT EXISTS trg_config_active_project_fk_insert
BEFORE INSERT ON config
FOR EACH ROW
WHEN NEW.active_project_slug IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM projects WHERE slug = NEW.active_project_slug)
BEGIN
    SELECT RAISE(ABORT, 'active_project_slug does not exist in projects');
END;

CREATE TRIGGER IF NOT EXISTS trg_config_active_project_fk_update
BEFORE UPDATE ON config
FOR EACH ROW
WHEN NEW.active_project_slug IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM projects WHERE slug = NEW.active_project_slug)
BEGIN
    SELECT RAISE(ABORT, 'active_project_slug does not exist in projects');
END;

CREATE TRIGGER IF NOT EXISTS trg_protect_active_project_delete
BEFORE DELETE ON projects
FOR EACH ROW
WHEN OLD.slug = (SELECT active_project_slug FROM config WHERE id = 1)
BEGIN
    SELECT RAISE(ABORT, 'cannot delete the active project');
END;
