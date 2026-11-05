CREATE TABLE agent_skills (
 id {{ID}},
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '',
 version BIGINT NOT NULL,
 source TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('draft','active','failed')),
 test_input TEXT NOT NULL DEFAULT '{}',
 test_output TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_by BIGINT NOT NULL,
 created_at BIGINT NOT NULL,
 updated_at BIGINT NOT NULL,
 UNIQUE(agent_id,bot_id,name,version)
);
CREATE INDEX agent_skills_catalog ON agent_skills(agent_id,bot_id,name,version DESC);

CREATE TABLE virtual_files (
 id {{ID}},
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL DEFAULT 0,
 chat_id BIGINT NOT NULL DEFAULT 0,
 user_id BIGINT NOT NULL DEFAULT 0,
 namespace TEXT NOT NULL CHECK(namespace IN ('user','chat','bot')),
 path TEXT NOT NULL,
 content TEXT NOT NULL DEFAULT '',
 mime_type TEXT NOT NULL DEFAULT 'text/plain',
 version BIGINT NOT NULL DEFAULT 1,
 created_at BIGINT NOT NULL,
 updated_at BIGINT NOT NULL,
 UNIQUE(agent_id,bot_id,chat_id,user_id,namespace,path)
);
CREATE INDEX virtual_files_browser ON virtual_files(agent_id,namespace,bot_id,chat_id,user_id,path);
