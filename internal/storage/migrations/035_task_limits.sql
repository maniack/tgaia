ALTER TABLE scheduled_tasks ADD COLUMN max_runs BIGINT NOT NULL DEFAULT 0 CHECK (max_runs>=0);
ALTER TABLE scheduled_tasks ADD COLUMN run_count BIGINT NOT NULL DEFAULT 0 CHECK (run_count>=0);
ALTER TABLE scheduled_tasks ADD COLUMN until_at BIGINT NOT NULL DEFAULT 0 CHECK (until_at>=0);
