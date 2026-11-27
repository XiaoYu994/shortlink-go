-- 阶段 16：为 jobs 表增加可选 W3C traceparent 列，支持异步任务跨进程 Span Link 关联（ADR 0032）。

ALTER TABLE jobs
    ADD COLUMN traceparent TEXT NOT NULL DEFAULT '';

ALTER TABLE jobs
    ADD CONSTRAINT ck_jobs_traceparent_length CHECK (octet_length(traceparent) <= 128);

COMMENT ON COLUMN jobs.traceparent IS '可选 W3C traceparent 载体，供 worker 通过 Span Link 关联入队源请求';
