CREATE TABLE agents (
 agent_id TEXT PRIMARY KEY
);
INSERT INTO agents (agent_id) SELECT agent_id FROM queue_scopes UNION SELECT agent_id FROM bot_messages;
CREATE TABLE telegram_bots (
 agent_id TEXT NOT NULL REFERENCES agents (agent_id),
 bot_id BIGINT NOT NULL CHECK (bot_id >= 0),
 credential_ref TEXT NOT NULL,
 PRIMARY KEY (agent_id, bot_id)
);
CREATE UNIQUE INDEX telegram_bot_owner ON telegram_bots (bot_id) WHERE bot_id > 0;
INSERT INTO telegram_bots (agent_id,bot_id,credential_ref) SELECT agent_id,0,'' FROM agents;
CREATE TABLE blocked_users (
 agent_id TEXT NOT NULL REFERENCES agents (agent_id),
 user_id BIGINT NOT NULL CHECK (user_id > 0),
 PRIMARY KEY (agent_id,user_id)
);
CREATE TABLE queue_scopes_v3 (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 PRIMARY KEY (agent_id,bot_id,chat_id),
 FOREIGN KEY (agent_id,bot_id) REFERENCES telegram_bots (agent_id,bot_id)
);
INSERT INTO queue_scopes_v3 (agent_id,bot_id,chat_id) SELECT agent_id,0,chat_id FROM queue_scopes;
CREATE TABLE queue_jobs_v3 (
 id {{ID}},
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 actor_user_id BIGINT NOT NULL DEFAULT 0 CHECK (actor_user_id >= 0),
 dedupe_key TEXT NOT NULL,
 kind TEXT NOT NULL,
 payload TEXT NOT NULL,
 status TEXT NOT NULL CHECK (status IN ('pending','leased','retry','completed','dead')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 max_attempts INTEGER NOT NULL CHECK (max_attempts BETWEEN 1 AND 100),
 available_at BIGINT NOT NULL,
 lease_until BIGINT NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '',
 created_at BIGINT NOT NULL,
 updated_at BIGINT NOT NULL,
 UNIQUE (agent_id,bot_id,chat_id,dedupe_key),
 FOREIGN KEY (agent_id,bot_id,chat_id) REFERENCES queue_scopes_v3 (agent_id,bot_id,chat_id)
);
INSERT INTO queue_jobs_v3 (id,agent_id,bot_id,chat_id,dedupe_key,kind,payload,status,attempts,max_attempts,available_at,lease_until,lease_token,created_at,updated_at)
 SELECT id,agent_id,0,chat_id,dedupe_key,kind,payload,status,attempts,max_attempts,available_at,lease_until,lease_token,created_at,updated_at FROM queue_jobs;
DROP TABLE queue_jobs;
DROP TABLE queue_scopes;
ALTER TABLE queue_scopes_v3 RENAME TO queue_scopes;
ALTER TABLE queue_jobs_v3 RENAME TO queue_jobs;
{{SEQUENCE_RESET}};
CREATE INDEX queue_ready ON queue_jobs (status,available_at,id);
CREATE INDEX queue_expired ON queue_jobs (status,lease_until,id);
CREATE INDEX queue_scope_active ON queue_jobs (agent_id,bot_id,chat_id,id) WHERE status IN ('pending','leased','retry');
CREATE INDEX queue_actor ON queue_jobs (agent_id,actor_user_id,status);
CREATE TABLE bot_messages_v3 (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL CHECK (message_id > 0),
 delivery_key TEXT NOT NULL,
 part INTEGER NOT NULL CHECK (part >= 0),
 sent_at BIGINT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
 edited_at BIGINT,
 deleted_at BIGINT,
 PRIMARY KEY (agent_id,bot_id,chat_id,message_id),
 FOREIGN KEY (agent_id,bot_id) REFERENCES telegram_bots (agent_id,bot_id)
);
INSERT INTO bot_messages_v3 (agent_id,bot_id,chat_id,message_id,delivery_key,part,sent_at,version,edited_at,deleted_at)
 SELECT agent_id,0,chat_id,message_id,delivery_key,part,sent_at,version,edited_at,deleted_at FROM bot_messages;
DROP TABLE bot_messages;
ALTER TABLE bot_messages_v3 RENAME TO bot_messages;
CREATE INDEX bot_messages_delivery ON bot_messages (agent_id,bot_id,chat_id,delivery_key,message_id);
