-- A user message remembers which message it replied to, so the model can see
-- what "this" refers to even when the quoted message left the history window.
ALTER TABLE conversation_messages ADD COLUMN reply_to_message_id BIGINT NOT NULL DEFAULT 0;
