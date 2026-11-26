-- perf-only (perf/instrumentation branch): adds the job timeline columns
-- to a job_records table created before them. 001 already has them for a
-- fresh database; this makes an existing one match.
ALTER TABLE tachyon.job_records
    ADD COLUMN IF NOT EXISTS host_received_at Nullable(DateTime64(3, 'UTC')) AFTER host_assigned_at,
    ADD COLUMN IF NOT EXISTS unattributed_seconds Nullable(Float64),
    ADD COLUMN IF NOT EXISTS timeline String CODEC(ZSTD(3));
