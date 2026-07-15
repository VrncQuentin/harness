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