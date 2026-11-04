CREATE TABLE explicit_memories (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 thread_id BIGINT NOT NULL DEFAULT 0,
 source_message_id BIGINT NOT NULL,
 source_update_id BIGINT NOT NULL,
 created_by BIGINT NOT NULL CHECK(created_by>0),
 content TEXT NOT NULL,
 embedding_model TEXT NOT NULL DEFAULT '',
 embedding TEXT NOT NULL DEFAULT '[]',
 created_at BIGINT NOT NULL,
 PRIMARY KEY(agent_id,bot_id,chat_id,thread_id,source_message_id),
 FOREIGN KEY(agent_id,bot_id,chat_id,thread_id) REFERENCES chat_topics(agent_id,bot_id,chat_id,thread_id),
 FOREIGN KEY(agent_id,bot_id,chat_id,source_message_id) REFERENCES conversation_messages(agent_id,bot_id,chat_id,message_id)
);
CREATE INDEX explicit_memory_recent ON explicit_memories(agent_id,bot_id,chat_id,thread_id,created_at DESC,source_message_id DESC);
