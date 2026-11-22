CREATE TABLE memory_episodes (
    id           {{ID}},
    agent_id     TEXT   NOT NULL,
    bot_id       BIGINT NOT NULL,
    chat_id      BIGINT NOT NULL,
    kind         TEXT   NOT NULL,
    period_start BIGINT NOT NULL DEFAULT 0,
    through_id   BIGINT NOT NULL,
    content      TEXT   NOT NULL,
    created_at   BIGINT NOT NULL,
    FOREIGN KEY (agent_id, bot_id, chat_id)
        REFERENCES chat_directory (agent_id, bot_id, chat_id)
);
CREATE UNIQUE INDEX memory_episodes_scope
    ON memory_episodes (agent_id, bot_id, chat_id, kind, period_start);
INSERT INTO memory_episodes (agent_id, bot_id, chat_id, kind, period_start, through_id, content, created_at)
SELECT agent_id, bot_id, chat_id, 'background', 0, through_id, content, 0
FROM memory_summaries;
DROP TABLE memory_summaries;
