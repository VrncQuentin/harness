ALTER TABLE config ADD COLUMN prompt_recency_n INTEGER NOT NULL DEFAULT 5;
ALTER TABLE config ADD COLUMN prompt_summarizer_prompt TEXT NOT NULL DEFAULT '';
