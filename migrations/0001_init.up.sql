CREATE TABLE IF NOT EXISTS config (
    id                           INTEGER PRIMARY KEY CHECK (id = 1),
    model_binary                 TEXT    NOT NULL DEFAULT '',
    model_path                   TEXT    NOT NULL DEFAULT '',
    model_ctx_size               INTEGER NOT NULL DEFAULT 32768,
    model_gpu_layers             INTEGER NOT NULL DEFAULT 35,
    model_n_parallel             INTEGER NOT NULL DEFAULT 1,
    model_port                   INTEGER NOT NULL DEFAULT 8081,
    embedder_binary              TEXT    NOT NULL DEFAULT '',
    embedder_model_path          TEXT    NOT NULL DEFAULT '',
    embedder_port                INTEGER NOT NULL DEFAULT 8082,
    memory_repo_path             TEXT    NOT NULL DEFAULT '',
    ui_port                      INTEGER NOT NULL DEFAULT 3000,
    ui_open_on_start             INTEGER NOT NULL DEFAULT 1,
    api_enabled                  INTEGER NOT NULL DEFAULT 0,
    api_port                     INTEGER NOT NULL DEFAULT 8080,
    prompt_ctx_size              INTEGER NOT NULL DEFAULT 32768,
    prompt_memory_token_budget   INTEGER NOT NULL DEFAULT 6144,
    prompt_conversation_reserve  INTEGER NOT NULL DEFAULT 8192,
    queue_max_depth              INTEGER NOT NULL DEFAULT 8,
    queue_wal_path               TEXT    NOT NULL DEFAULT '',
    metrics_retention_days       INTEGER NOT NULL DEFAULT 30,
    saved_at                     INTEGER
);

CREATE TABLE IF NOT EXISTS metrics (
    id    INTEGER PRIMARY KEY AUTOINCREMENT,
    name  TEXT    NOT NULL,
    value REAL    NOT NULL,
    tags  TEXT,
    ts    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS metrics_name_ts ON metrics(name, ts);
