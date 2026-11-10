-- Backfill thread_id=0 for rows written before migration 020 added the
-- is_forum+is_topic_message guard to Incoming(). Telegram sets message_thread_id
-- on replies in any supergroup, but only forum topics are real topics. Rows in
-- non-forum chats with a stale non-zero thread_id caused edited-message identity
-- conflicts and broken reply-link lookups.

-- explicit_memories: PRIMARY KEY(...,thread_id,source_message_id), FK to chat_topics.
-- Step 1: drop stale rows where a thread_id=0 counterpart already exists.
DELETE FROM explicit_memories
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=explicit_memories.agent_id
   AND d.bot_id=explicit_memories.bot_id
   AND d.chat_id=explicit_memories.chat_id
   AND d.is_forum=FALSE
 )
 AND EXISTS (
  SELECT 1 FROM explicit_memories e2
  WHERE e2.agent_id=explicit_memories.agent_id
   AND e2.bot_id=explicit_memories.bot_id
   AND e2.chat_id=explicit_memories.chat_id
   AND e2.thread_id=0
   AND e2.source_message_id=explicit_memories.source_message_id
 );
-- Step 2: move remaining stale rows to thread_id=0 (FK now points to existing topic 0).
UPDATE explicit_memories SET thread_id=0
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=explicit_memories.agent_id
   AND d.bot_id=explicit_memories.bot_id
   AND d.chat_id=explicit_memories.chat_id
   AND d.is_forum=FALSE
 );

-- reply_links: PRIMARY KEY(...,thread_id,...) — delete stale non-zero rows.
-- thread_id=0 rows (written after the fix) are the correct ones and stay.
DELETE FROM reply_links
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=reply_links.agent_id
   AND d.bot_id=reply_links.bot_id
   AND d.chat_id=reply_links.chat_id
   AND d.is_forum=FALSE
 );

-- conversation_messages: UNIQUE(agent_id,bot_id,chat_id,message_id), thread_id not in key.
UPDATE conversation_messages SET thread_id=0
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=conversation_messages.agent_id
   AND d.bot_id=conversation_messages.bot_id
   AND d.chat_id=conversation_messages.chat_id
   AND d.is_forum=FALSE
 );

-- bot_messages: PRIMARY KEY(agent_id,bot_id,chat_id,message_id), thread_id not in key.
UPDATE bot_messages SET thread_id=0
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=bot_messages.agent_id
   AND d.bot_id=bot_messages.bot_id
   AND d.chat_id=bot_messages.chat_id
   AND d.is_forum=FALSE
 );

-- scheduled_tasks: PRIMARY KEY(id), thread_id not in key.
UPDATE scheduled_tasks SET thread_id=0
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=scheduled_tasks.agent_id
   AND d.bot_id=scheduled_tasks.bot_id
   AND d.chat_id=scheduled_tasks.chat_id
   AND d.is_forum=FALSE
 );

-- chat_topics: PRIMARY KEY(agent_id,bot_id,chat_id,thread_id).
-- Remove stale non-zero rows for non-forum chats (thread_id=0 rows remain).
-- Must run after explicit_memories is updated above (FK dependency).
DELETE FROM chat_topics
WHERE thread_id<>0
 AND EXISTS (
  SELECT 1 FROM chat_directory d
  WHERE d.agent_id=chat_topics.agent_id
   AND d.bot_id=chat_topics.bot_id
   AND d.chat_id=chat_topics.chat_id
   AND d.is_forum=FALSE
 );
