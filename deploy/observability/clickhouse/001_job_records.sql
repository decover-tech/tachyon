-- One row per finished job, written by the control plane over the HTTP
-- interface (INSERT ... FORMAT JSONEachRow; see internal/telemetry/row.go).
-- The same row drives billing, the customer's minutes-saved report and the
-- job-level Grafana dashboard. Column names must match telemetry.JobRow's
-- JSON tags; a test in this directory checks that.
--
-- Writes are at-least-once (the exporter retries a failed batch), so the
-- table is a ReplacingMergeTree keyed by job: query with FINAL, or group by
-- job_id, when an exact count matters.

CREATE DATABASE IF NOT EXISTS tachyon;

CREATE TABLE IF NOT EXISTS tachyon.job_records
(
    job_id              String,
    tenant              LowCardinality(String),
    repo                String,
    region              LowCardinality(String),
    host_id             LowCardinality(String),
    vcpu                UInt16,
    prebuild_match      LowCardinality(String),
    capacity            LowCardinality(String),
    outcome             LowCardinality(String),

    webhook_received_at Nullable(DateTime64(3, 'UTC')),
    host_assigned_at    Nullable(DateTime64(3, 'UTC')),
    vm_resumed_at       Nullable(DateTime64(3, 'UTC')),
    runner_online_at    Nullable(DateTime64(3, 'UTC')),
    job_started_at      Nullable(DateTime64(3, 'UTC')),
    job_finished_at     Nullable(DateTime64(3, 'UTC')),
    vm_destroyed_at     Nullable(DateTime64(3, 'UTC')),

    pickup_seconds      Nullable(Float64),
    run_seconds         Nullable(Float64),
    billable_seconds    Nullable(Float64),

    step_names          Array(String),
    step_seconds        Array(Float64),

    cache_hits          UInt32,
    cache_misses        UInt32,
    host_load           Float64,
    cost_usd            Float64,

    inserted_at         DateTime64(3, 'UTC') DEFAULT now64(3)
)
ENGINE = ReplacingMergeTree(inserted_at)
PARTITION BY toYYYYMM(inserted_at)
ORDER BY (tenant, repo, job_id)
TTL toDateTime(inserted_at) + INTERVAL 400 DAY;
