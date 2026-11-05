CREATE TABLE bot_reaction_states (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL,
 actor_type TEXT NOT NULL CHECK (actor_type IN ('user','sender_chat','anonymous')),
 actor_id BIGINT NOT NULL,
 update_id BIGINT NOT NULL CHECK (update_id >= 0),
 actor_name TEXT NOT NULL DEFAULT '',
 actor_username TEXT NOT NULL DEFAULT '',
 updated_at BIGINT NOT NULL,
 PRIMARY KEY (agent_id,bot_id,chat_id,message_id,actor_type,actor_id),
 FOREIGN KEY (agent_id,bot_id,chat_id,message_id) REFERENCES bot_messages (agent_id,bot_id,chat_id,message_id) ON DELETE CASCADE
);

CREATE TABLE bot_message_reactions (
 agent_id TEXT NOT NULL,
 bot_id BIGINT NOT NULL,
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL,
 actor_type TEXT NOT NULL,
 actor_id BIGINT NOT NULL,
 reaction_type TEXT NOT NULL,
 reaction_value TEXT NOT NULL DEFAULT '',
 total_count INTEGER NOT NULL CHECK (total_count > 0),
 PRIMARY KEY (agent_id,bot_id,chat_id,message_id,actor_type,actor_id,reaction_type,reaction_value),
 FOREIGN KEY (agent_id,bot_id,chat_id,message_id,actor_type,actor_id)
  REFERENCES bot_reaction_states (agent_id,bot_id,chat_id,message_id,actor_type,actor_id) ON DELETE CASCADE
);

CREATE INDEX bot_reactions_context ON bot_reaction_states (agent_id,bot_id,chat_id,updated_at DESC,message_id DESC);
