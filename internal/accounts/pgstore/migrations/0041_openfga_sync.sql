-- How far OpenFGA has caught up with each organisation's access policy
-- (docs/plans/OPENFGA_PLAN.md, phase 5). A row says which policy revision
-- OpenFGA accepted, in which store and under which model. It is separate
-- from access_policy_outbox, which the graph projection consumes.
CREATE TABLE openfga_sync (
    org_id text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    store_id text NOT NULL DEFAULT '',
    model_id text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 0,
    synced_at timestamptz,
    rebuild_requested_at timestamptz
);
