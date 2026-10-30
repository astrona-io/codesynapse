-- Invitations to join an organisation. Only the SHA-256 of the one-time accept
-- token is kept. A row is removed when it is accepted, revoked or expired; the
-- audit log keeps the history.
CREATE TABLE org_invitations (
    id         text PRIMARY KEY,
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email      text NOT NULL,
    role       text NOT NULL,
    token_hash text NOT NULL UNIQUE,
    invited_by text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE INDEX org_invitations_org ON org_invitations (org_id, created_at);
