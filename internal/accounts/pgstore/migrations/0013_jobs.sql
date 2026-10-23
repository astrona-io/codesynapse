-- Durable jobs (docs/SAAS.md, 5e): queued work and job history survive
-- restarts. Parameters rebuild the work and are cleared when it ends; no
-- credentials are stored.
CREATE TABLE jobs (
    id          text PRIMARY KEY,
    kind        text NOT NULL,
    repo        text NOT NULL,
    org_id      text NOT NULL,
    actor_id    text NOT NULL,
    via         text NOT NULL,
    key_id      text NOT NULL DEFAULT '',
    role        text NOT NULL,
    execution   jsonb NOT NULL,
    params      bytea,
    branch      text NOT NULL DEFAULT '',
    task        text NOT NULL DEFAULT '',
    state       text NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed')),
    attempts    int NOT NULL DEFAULT 0,
    lease_owner text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL,
    created_at  timestamptz NOT NULL,
    started_at  timestamptz,
    finished_at timestamptz,
    result      text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    nodes       int NOT NULL DEFAULT 0,
    edges       int NOT NULL DEFAULT 0
);
-- One queued or running job per repository.
CREATE UNIQUE INDEX jobs_active_repo ON jobs (repo) WHERE state IN ('queued', 'running');
CREATE INDEX jobs_orphans ON jobs (lease_until) WHERE state IN ('queued', 'running');
CREATE INDEX jobs_org_created ON jobs (org_id, created_at DESC);
