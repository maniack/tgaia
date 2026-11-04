ALTER TABLE queue_jobs ADD COLUMN thread_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE queue_jobs ADD COLUMN sender_chat_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE conversation_messages ADD COLUMN thread_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE conversation_messages ADD COLUMN sender_chat_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE conversation_messages ADD COLUMN update_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE conversation_messages ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';
ALTER TABLE conversation_messages ADD COLUMN media_text TEXT NOT NULL DEFAULT '';
ALTER TABLE scheduled_tasks ADD COLUMN thread_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE bot_messages ADD COLUMN thread_id BIGINT NOT NULL DEFAULT 0;
CREATE TABLE chat_topics (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 thread_id BIGINT NOT NULL,
 PRIMARY KEY(agent_id,bot_id,chat_id,thread_id),
 FOREIGN KEY(agent_id,bot_id,chat_id) REFERENCES chat_directory(agent_id,bot_id,chat_id)
);
INSERT INTO chat_topics SELECT agent_id,bot_id,chat_id,0 FROM chat_directory;
CREATE TABLE memory_summaries_v11 (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 thread_id BIGINT NOT NULL DEFAULT 0,
 through_id BIGINT NOT NULL,
 content TEXT NOT NULL,
 PRIMARY KEY(agent_id,bot_id,chat_id,thread_id),
 FOREIGN KEY(agent_id,bot_id,chat_id) REFERENCES chat_directory(agent_id,bot_id,chat_id)
);
INSERT INTO memory_summaries_v11(agent_id,bot_id,chat_id,through_id,content) SELECT agent_id,bot_id,chat_id,through_id,content FROM memory_summaries;
DROP TABLE memory_summaries;
ALTER TABLE memory_summaries_v11 RENAME TO memory_summaries;
CREATE INDEX conversation_topic ON conversation_messages(agent_id,bot_id,chat_id,thread_id,id);
