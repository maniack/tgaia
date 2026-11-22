ALTER TABLE queue_jobs ADD COLUMN priority SMALLINT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 9);
CREATE INDEX queue_scope_priority ON queue_jobs (agent_id,bot_id,chat_id,priority,id) WHERE status IN ('pending','leased','retry');
