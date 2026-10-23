-- Auto-sync schedules shared by every API process (docs/SAAS.md, 5e), and
-- leases that let one process run the singleton loops (scheduler, pruning).
CREATE TABLE schedules (
    repo             text PRIMARY KEY,
    org_id           text NOT NULL DEFAULT '',
    actor_id         text NOT NULL DEFAULT '',
    execution        jsonb,
    interval_minutes int NOT NULL,
    paused           boolean NOT NULL DEFAULT false,
    auto_review      boolean NOT NULL DEFAULT false,
    auto_triage      boolean NOT NULL DEFAULT false,
    last_run         timestamptz,
    last_status      text NOT NULL DEFAULT '',
    last_error       text NOT NULL DEFAULT '',
    missing          int NOT NULL DEFAULT 0,
    next_run         timestamptz
);
CREATE INDEX schedules_due ON schedules (next_run) WHERE next_run IS NOT NULL;

CREATE TABLE leases (
    name  text PRIMARY KEY,
    owner text NOT NULL,
    until timestamptz NOT NULL
);
