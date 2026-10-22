-- Runtime chat IDs are supplied upstream. Closed IDs remain reserved to prevent replay.
CREATE TABLE execution_sessions (
    id text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    user_id text NOT NULL REFERENCES users(id),
    org_id text NOT NULL REFERENCES orgs(id),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz
);
CREATE INDEX execution_sessions_active_org ON execution_sessions (org_id, expires_at) WHERE revoked_at IS NULL;
CREATE INDEX execution_sessions_org ON execution_sessions (org_id);
