CREATE TABLE queue_paused_scopes (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL DEFAULT 0,
 chat_id BIGINT NOT NULL,
 paused_at BIGINT NOT NULL,
 paused_by BIGINT NOT NULL DEFAULT 0,
 reason TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (agent_id, bot_id, chat_id)
);
