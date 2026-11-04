-- Memory is shared across forum topics of one chat: a single summary per
-- agent/bot/chat (stored in topic 0) and explicit memories read chat-wide.
-- Keep the most advanced summary of each chat and retire the per-topic rest.
DELETE FROM memory_summaries WHERE EXISTS (
 SELECT 1 FROM memory_summaries s WHERE s.agent_id=memory_summaries.agent_id AND s.bot_id=memory_summaries.bot_id AND s.chat_id=memory_summaries.chat_id
 AND (s.through_id>memory_summaries.through_id OR (s.through_id=memory_summaries.through_id AND s.thread_id<memory_summaries.thread_id))
);
UPDATE memory_summaries SET thread_id=0 WHERE thread_id<>0;
