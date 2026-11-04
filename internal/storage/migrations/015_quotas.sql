CREATE TABLE usage_quotas (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 category TEXT NOT NULL,
 bucket BIGINT NOT NULL,
 used BIGINT NOT NULL,
 PRIMARY KEY(agent_id,bot_id,chat_id,category)
);
