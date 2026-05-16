CREATE TABLE IF NOT EXISTS projects (
    slug             TEXT PRIMARY KEY,
    display_name     TEXT NOT NULL,
    model_binary     TEXT,
    model_path       TEXT,
    model_ctx_size   INTEGER,
    model_gpu_layers INTEGER,
    model_n_parallel INTEGER,
    hidden           INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    saved_at         INTEGER
);

-- Seed global project before adding the referencing config column so the
-- default value and any subsequent inserts pass referential checks.
INSERT OR IGNORE INTO projects (slug, display_name, hidden, created_at) VALUES ('global', 'Global', 0, CAST(strftime('%s', 'now') AS INTEGER));

CREATE TABLE IF NOT EXISTS project_directories (
    project_slug TEXT NOT NULL,
    path         TEXT NOT NULL,
    PRIMARY KEY (project_slug, path),
    FOREIGN KEY (project_slug) REFERENCES projects(slug) ON DELETE CASCADE
);

ALTER TABLE config ADD COLUMN active_project_slug TEXT NOT NULL DEFAULT 'global';
ALTER TABLE config ADD COLUMN project_llama_on_switch TEXT NOT NULL DEFAULT 'reload';

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
