ALTER TABLE projects ADD COLUMN memory_repo_path TEXT;

UPDATE projects SET memory_repo_path = '' WHERE memory_repo_path IS NULL;
