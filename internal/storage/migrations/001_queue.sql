CREATE TABLE queue_scopes (
 agent_id TEXT NOT NULL,
 chat_id BIGINT NOT NULL,
 PRIMARY KEY (agent_id, chat_id)
);
CREATE TABLE queue_jobs (
 id {{ID}},
 agent_id TEXT NOT NULL,
 chat_id BIGINT NOT NULL,
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
 UNIQUE (agent_id, chat_id, dedupe_key),
 FOREIGN KEY (agent_id, chat_id) REFERENCES queue_scopes (agent_id, chat_id)
);
CREATE INDEX queue_ready ON queue_jobs (status, available_at, id);
CREATE INDEX queue_expired ON queue_jobs (status, lease_until, id);
CREATE INDEX queue_scope_active ON queue_jobs (agent_id, chat_id, id) WHERE status IN ('pending','leased','retry');
