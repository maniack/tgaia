ALTER TABLE telegram_bots ADD COLUMN system_prompt TEXT NOT NULL DEFAULT '';
CREATE TABLE runtime_settings (
 name TEXT PRIMARY KEY,
 value TEXT NOT NULL,
 secret BOOLEAN NOT NULL,
 version BIGINT NOT NULL DEFAULT 1
);
