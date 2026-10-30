-- Audit statistics (docs/PLATFORM_STATS_PLAN.md, phase 7). The 24-hour
-- figures count events by time and action; the existing indexes are on at
-- and action apart, so the planner had to read every row of the window.
CREATE INDEX events_at_action ON events (at, action);

-- Stored results of the audit chain verification: run once a day by the
-- process holding the api-loops lease and on demand by a platform admin.
-- The statistics read the latest result instead of walking the chain on
-- every view. Counts and IDs only; reason is one of the verifier's constant
-- texts. Kept 400 days (pruned on insert).
CREATE TABLE audit_verifications (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL,
    trigger     text NOT NULL CHECK (trigger IN ('scheduled', 'on_demand')),
    deep        boolean NOT NULL DEFAULT false,
    duration_ms bigint NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    run_error   boolean NOT NULL DEFAULT false,
    ok          boolean NOT NULL,
    checked     bigint NOT NULL DEFAULT 0 CHECK (checked >= 0),
    broken_at   bigint NOT NULL DEFAULT 0,
    reason      text NOT NULL DEFAULT '' CHECK (length(reason) <= 200),
    first_id    bigint NOT NULL DEFAULT 0,
    segments    integer NOT NULL DEFAULT 0 CHECK (segments >= 0),
    sealed      bigint NOT NULL DEFAULT 0 CHECK (sealed >= 0),
    archives    integer NOT NULL DEFAULT 0 CHECK (archives >= 0)
);
CREATE INDEX audit_verifications_at ON audit_verifications (at DESC);
