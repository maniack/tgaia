ALTER TABLE chat_directory ADD COLUMN is_forum BOOLEAN NOT NULL DEFAULT FALSE;
INSERT INTO chat_topics(agent_id,bot_id,chat_id,thread_id)
 SELECT agent_id,bot_id,chat_id,0 FROM chat_directory WHERE TRUE ON CONFLICT DO NOTHING;
