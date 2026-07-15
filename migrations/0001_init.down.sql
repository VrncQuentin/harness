DROP TRIGGER IF EXISTS trg_protect_active_project_delete;
DROP TRIGGER IF EXISTS trg_config_active_project_fk_update;
DROP TRIGGER IF EXISTS trg_config_active_project_fk_insert;
DROP TRIGGER IF EXISTS trg_protect_global_project_hide;
DROP TRIGGER IF EXISTS trg_protect_global_project_slug_update;
DROP TRIGGER IF EXISTS trg_protect_global_project_delete;

DROP INDEX IF EXISTS metrics_hourly_name_hour;
DROP TABLE IF EXISTS metrics_hourly;
DROP INDEX IF EXISTS metrics_name_ts;
DROP TABLE IF EXISTS metrics;
DROP TABLE IF EXISTS config;
DROP TABLE IF EXISTS project_directories;
DROP TABLE IF EXISTS projects;
