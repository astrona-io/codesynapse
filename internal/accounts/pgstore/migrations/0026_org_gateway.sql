-- Organisations opt in to the agent gateway (default off): the gateway sends
-- agent model traffic through CodeSynapse, so an owner or admin turns it on.
CREATE TABLE org_gateway (
    org_id     text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    enabled    boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);
