DROP TRIGGER IF EXISTS trg_config_active_project_fk_update;
DROP TRIGGER IF EXISTS trg_config_active_project_fk_insert;
DROP TRIGGER IF EXISTS trg_protect_global_project_hide;
DROP TRIGGER IF EXISTS trg_protect_global_project_slug_update;
DROP TRIGGER IF EXISTS trg_protect_global_project_delete;

ALTER TABLE config DROP COLUMN active_project_slug;
ALTER TABLE config DROP COLUMN project_llama_on_switch;

DROP TABLE IF EXISTS project_directories;
DROP TABLE IF EXISTS projects;
