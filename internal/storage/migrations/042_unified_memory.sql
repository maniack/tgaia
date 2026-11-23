ALTER TABLE memory_episodes ADD COLUMN source_message_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_episodes ADD COLUMN source_update_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_episodes ADD COLUMN created_by BIGINT NOT NULL DEFAULT 0;
ALTER TABLE memory_episodes ADD COLUMN embedding_model TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_episodes ADD COLUMN embedding TEXT NOT NULL DEFAULT '[]';

INSERT INTO memory_episodes
  (agent_id, bot_id, chat_id, kind, period_start, through_id,
   source_message_id, source_update_id, created_by, content,
   embedding_model, embedding, created_at)
SELECT
  agent_id, bot_id, chat_id, 'fact', source_message_id, source_message_id,
  source_message_id, source_update_id, created_by, content,
  COALESCE(embedding_model, ''), COALESCE(embedding, '[]'), created_at
FROM explicit_memories;

DROP TABLE explicit_memories;
