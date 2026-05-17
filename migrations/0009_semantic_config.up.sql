ALTER TABLE config ADD COLUMN prompt_semantic_weight REAL NOT NULL DEFAULT 0.5;
ALTER TABLE config ADD COLUMN prompt_recency_weight REAL NOT NULL DEFAULT 0.5;
