CREATE TABLE audit_events (
 id {{ID}},
 subject TEXT NOT NULL,
 operation TEXT NOT NULL,
 request TEXT NOT NULL,
 status INTEGER NOT NULL,
 created_at BIGINT NOT NULL
);
ALTER TABLE audit_events ADD COLUMN before_state TEXT NOT NULL DEFAULT '{}';
ALTER TABLE audit_events ADD COLUMN after_state TEXT NOT NULL DEFAULT '{}';
