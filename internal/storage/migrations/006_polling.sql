CREATE TABLE bot_poll_leases (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 offset_id BIGINT NOT NULL DEFAULT 0,
 token TEXT NOT NULL DEFAULT '',
 lease_until BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY (agent_id,bot_id),
 FOREIGN KEY (agent_id,bot_id) REFERENCES telegram_bots (agent_id,bot_id)
);
