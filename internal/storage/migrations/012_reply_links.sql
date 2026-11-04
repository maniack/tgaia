CREATE TABLE reply_links (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 thread_id BIGINT NOT NULL DEFAULT 0,
 source_message_id BIGINT NOT NULL,
 part INTEGER NOT NULL,
 message_id BIGINT NOT NULL,
 PRIMARY KEY(agent_id,bot_id,chat_id,thread_id,source_message_id,part),
 FOREIGN KEY(agent_id,bot_id,chat_id,message_id) REFERENCES bot_messages(agent_id,bot_id,chat_id,message_id)
);
INSERT INTO reply_links(agent_id,bot_id,chat_id,thread_id,source_message_id,part,message_id)
 SELECT agent_id,bot_id,chat_id,thread_id,source_message_id,part,message_id FROM (
 SELECT j.agent_id,j.bot_id,j.chat_id,j.thread_id,{{SOURCE_MESSAGE_ID}} AS source_message_id,d.part,d.message_id,
 ROW_NUMBER() OVER(PARTITION BY j.agent_id,j.bot_id,j.chat_id,j.thread_id,{{SOURCE_MESSAGE_ID}},d.part ORDER BY j.id DESC) AS ordinal
 FROM outbound_deliveries d JOIN queue_jobs j ON j.id=d.job_id
 WHERE d.state='sent' AND d.message_id IS NOT NULL AND j.kind='respond'
 ) AS known_replies WHERE ordinal=1 AND source_message_id>0;
