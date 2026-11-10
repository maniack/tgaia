CREATE TABLE choice_sessions (
  id              {{ID}},
  agent_id        TEXT    NOT NULL,
  bot_id          INTEGER NOT NULL,
  chat_id         INTEGER NOT NULL,
  thread_id       INTEGER NOT NULL DEFAULT 0,
  user_id         INTEGER NOT NULL,
  message_id      INTEGER NOT NULL DEFAULT 0,
  question        TEXT    NOT NULL DEFAULT '',
  options         TEXT    NOT NULL DEFAULT '[]',
  multi           INTEGER NOT NULL DEFAULT 0,
  selected        TEXT    NOT NULL DEFAULT '[]',
  state           TEXT    NOT NULL DEFAULT 'open',
  context_cursor  INTEGER NOT NULL DEFAULT 0,
  job_id          INTEGER,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL
);
CREATE INDEX choice_sessions_state ON choice_sessions(state, expires_at);
