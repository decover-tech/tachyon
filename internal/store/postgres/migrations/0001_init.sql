-- Control plane state. Each row keeps the full Go value in `data` (jsonb);
-- the other columns are what queries filter and order by.

CREATE TABLE jobs (
    id         text PRIMARY KEY,
    state      text NOT NULL,
    host_id    text NOT NULL DEFAULT '',
    data       jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_open ON jobs (id) WHERE state IN ('queued', 'deferred', 'assigned');

CREATE TABLE hosts (
    id        text PRIMARY KEY,
    data      jsonb NOT NULL,
    last_seen timestamptz NOT NULL
);

CREATE TABLE prebuilds (
    seq        bigserial,
    repo       text NOT NULL,
    key        text NOT NULL,
    commit     text NOT NULL,
    created_at timestamptz NOT NULL,
    healthy    boolean NOT NULL,
    data       jsonb NOT NULL,
    PRIMARY KEY (repo, key, commit)
);
CREATE INDEX prebuilds_latest_healthy ON prebuilds (repo, created_at DESC, seq DESC) WHERE healthy;

CREATE TABLE prebuild_configs (
    repo       text PRIMARY KEY,
    data       jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE job_records (
    job_id     text PRIMARY KEY,
    tenant     text NOT NULL,
    repo       text NOT NULL,
    data       jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX job_records_tenant ON job_records (tenant, updated_at);

CREATE TABLE pending_runners (
    frontend    text NOT NULL,
    external_id bigint NOT NULL,
    data        jsonb NOT NULL,
    PRIMARY KEY (frontend, external_id)
);

CREATE TABLE active_runners (
    frontend        text NOT NULL,
    name            text NOT NULL,
    installation_id bigint NOT NULL,
    repo            text NOT NULL,
    PRIMARY KEY (frontend, name)
);
