CREATE TABLE behavior_corrections (
 id {{ID}},
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 text TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK (status IN ('pending','approved','disabled')),
 created_at BIGINT NOT NULL,
 updated_at BIGINT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX behavior_corrections_scope ON behavior_corrections (agent_id,bot_id,chat_id,status);
ALTER TABLE chat_directory ADD COLUMN auto_approve_corrections BOOLEAN NOT NULL DEFAULT FALSE;
