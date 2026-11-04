CREATE TABLE bot_messages (
 agent_id TEXT NOT NULL,
 chat_id BIGINT NOT NULL,
 message_id BIGINT NOT NULL CHECK (message_id > 0),
 delivery_key TEXT NOT NULL,
 part INTEGER NOT NULL CHECK (part >= 0),
 sent_at BIGINT NOT NULL,
 version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
 edited_at BIGINT,
 deleted_at BIGINT,
 PRIMARY KEY (agent_id, chat_id, message_id)
);
CREATE INDEX bot_messages_delivery ON bot_messages (agent_id, chat_id, delivery_key, message_id);
