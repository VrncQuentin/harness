CREATE TABLE IF NOT EXISTS config (
    id                           INTEGER PRIMARY KEY CHECK (id = 1),
    model_binary                 TEXT    NOT NULL,
    model_path                   TEXT    NOT NULL,
    model_ctx_size               INTEGER NOT NULL,
    model_gpu_layers             INTEGER NOT NULL,
    model_n_parallel             INTEGER NOT NULL,
    model_port                   INTEGER NOT NULL,
    embedder_binary              TEXT    NOT NULL,
    embedder_model_path          TEXT    NOT NULL,
    embedder_port                INTEGER NOT NULL,
    ui_port                      INTEGER NOT NULL,
    ui_open_on_start             INTEGER NOT NULL,
    api_enabled                  INTEGER NOT NULL,
    api_port                     INTEGER NOT NULL,
    prompt_ctx_size              INTEGER NOT NULL,
    prompt_memory_token_budget   INTEGER NOT NULL,
    prompt_conversation_reserve  INTEGER NOT NULL,
    queue_max_depth              INTEGER NOT NULL,
    queue_wal_path               TEXT    NOT NULL,
    metrics_retention_days       INTEGER NOT NULL,
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
