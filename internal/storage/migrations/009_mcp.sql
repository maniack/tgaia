CREATE TABLE tool_steps (
 job_id BIGINT NOT NULL REFERENCES queue_jobs (id),
 step TEXT NOT NULL,
 request TEXT NOT NULL,
 state TEXT NOT NULL CHECK (state IN ('started','completed','uncertain')),
 result TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (job_id,step)
);
