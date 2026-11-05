ALTER TABLE chat_directory ADD COLUMN group_response TEXT NOT NULL DEFAULT 'mentions' CHECK (group_response IN ('mentions','all'));
ALTER TABLE chat_directory ADD COLUMN respond_to_bots BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE chat_directory ADD COLUMN loop_protection BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE chat_directory ADD COLUMN behavior_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE seen_users ADD COLUMN is_bot BOOLEAN NOT NULL DEFAULT FALSE;
