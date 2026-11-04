CREATE TABLE telegram_inbox (
 bot_id BIGINT NOT NULL,
 update_id BIGINT NOT NULL,
 PRIMARY KEY (bot_id,update_id)
);
CREATE TABLE chat_directory (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 title TEXT NOT NULL,
 kind TEXT NOT NULL,
 blocked BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (agent_id,bot_id,chat_id),
 FOREIGN KEY (agent_id,bot_id) REFERENCES telegram_bots (agent_id,bot_id)
);
CREATE TABLE seen_users (
 agent_id TEXT NOT NULL REFERENCES agents (agent_id),
 user_id BIGINT NOT NULL,
 username TEXT NOT NULL,
 name TEXT NOT NULL,
 PRIMARY KEY (agent_id,user_id)
);
CREATE TABLE conversation_messages (
 id {{ID}},
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL,
 user_id BIGINT NOT NULL,
 role TEXT NOT NULL,
 text TEXT NOT NULL,
 sent_at BIGINT NOT NULL,
 UNIQUE (agent_id,bot_id,chat_id,message_id),
 FOREIGN KEY (agent_id,bot_id,chat_id) REFERENCES chat_directory (agent_id,bot_id,chat_id)
);
CREATE INDEX conversation_scope ON conversation_messages (agent_id,bot_id,chat_id,id);
CREATE TABLE memory_summaries (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 through_id BIGINT NOT NULL,
 content TEXT NOT NULL,
 PRIMARY KEY (agent_id,bot_id,chat_id),
 FOREIGN KEY (agent_id,bot_id,chat_id) REFERENCES chat_directory (agent_id,bot_id,chat_id)
);
CREATE TABLE shared_knowledge (
 agent_id TEXT PRIMARY KEY REFERENCES agents (agent_id),
 content TEXT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1
);
CREATE TABLE scheduled_tasks (
 id TEXT PRIMARY KEY,
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 user_id BIGINT NOT NULL CHECK (user_id>0),
 prompt TEXT NOT NULL,
 run_at BIGINT NOT NULL,
 interval_ms BIGINT NOT NULL CHECK (interval_ms>=0),
 active BOOLEAN NOT NULL,
 version BIGINT NOT NULL DEFAULT 1,
 FOREIGN KEY (agent_id,bot_id,chat_id) REFERENCES chat_directory (agent_id,bot_id,chat_id)
);
CREATE INDEX task_due ON scheduled_tasks (active,run_at,id);
CREATE TABLE scheduler_lock (id INTEGER PRIMARY KEY, version INTEGER NOT NULL);
INSERT INTO scheduler_lock (id,version) VALUES (1,0);
CREATE TABLE job_results (
 job_id BIGINT PRIMARY KEY REFERENCES queue_jobs (id),
 result TEXT NOT NULL
);
CREATE TABLE outbound_deliveries (
 job_id BIGINT NOT NULL REFERENCES queue_jobs (id),
 part INTEGER NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('sending','sent','uncertain')),
 text TEXT NOT NULL,
 message_id BIGINT,
 PRIMARY KEY (job_id,part)
);
