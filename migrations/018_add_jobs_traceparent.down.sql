ALTER TABLE jobs
    DROP CONSTRAINT IF EXISTS ck_jobs_traceparent_length,
    DROP COLUMN IF EXISTS traceparent;
