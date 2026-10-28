-- Daily platform statistics (docs/PLATFORM_STATS_PLAN.md, "Daily rollup"):
-- counts per UTC day, metric and dimension, per organisation or (org_id
-- empty) for the platform. The metric and dimension lists live in the code
-- (accounts.ValidPlatformStat), which refuses anything else; the checks
-- below keep the columns to short identifiers and counts. No foreign key on
-- org_id: a deleted organisation's rows are kept. Kept 400 days.
CREATE TABLE platform_stats_daily (
    day       date NOT NULL,
    metric    text NOT NULL CHECK (metric ~ '^[a-z_]{1,40}$'),
    dimension text NOT NULL CHECK (dimension ~ '^[a-z0-9_.-]{1,40}$'),
    org_id    text NOT NULL DEFAULT '' CHECK (length(org_id) <= 128),
    value     bigint NOT NULL CHECK (value >= 0),
    PRIMARY KEY (day, metric, dimension, org_id)
);
CREATE INDEX platform_stats_daily_org ON platform_stats_daily (org_id, day) WHERE org_id <> '';

-- The writer counts the jobs that finished on a day.
CREATE INDEX jobs_finished_at ON jobs (finished_at) WHERE finished_at IS NOT NULL;
