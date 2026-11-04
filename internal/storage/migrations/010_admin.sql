ALTER TABLE outbound_deliveries ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE tool_steps ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
CREATE TABLE outcome_resolutions (
 id {{ID}},
 job_id BIGINT NOT NULL REFERENCES queue_jobs(id),
 target TEXT NOT NULL,
 action TEXT NOT NULL,
 subject TEXT NOT NULL,
 reason TEXT NOT NULL,
 created_at BIGINT NOT NULL,
 UNIQUE(job_id,target)
);
