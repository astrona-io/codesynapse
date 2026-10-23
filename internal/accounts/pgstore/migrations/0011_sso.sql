-- Single sign-on (docs/SSO_PLAN.md): one OpenID Connect provider per
-- organisation, the external identities linked to accounts, short-lived
-- login states and the memberships a provider manages.
CREATE TABLE identity_providers (
    org_id         text PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    name           text NOT NULL,
    issuer         text NOT NULL,
    client_id      text NOT NULL,
    secret_preview text NOT NULL,
    key_id         text NOT NULL,
    sealed_secret  bytea NOT NULL,
    -- scopes, groups claim, default role and group mappings (bounded JSON).
    settings       jsonb NOT NULL,
    enabled        boolean NOT NULL,
    created_by     text NOT NULL REFERENCES users (id),
    updated_at     timestamptz NOT NULL
);

CREATE TABLE external_identities (
    id            text PRIMARY KEY,
    org_id        text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    issuer        text NOT NULL,
    subject       text NOT NULL,
    user_id       text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    email         text NOT NULL,
    linked_at     timestamptz NOT NULL,
    last_login_at timestamptz,
    UNIQUE (issuer, subject)
);
CREATE INDEX external_identities_user ON external_identities (user_id);

-- Only hashes: the state and the browser binder are secrets until used.
CREATE TABLE sso_states (
    state_hash  text PRIMARY KEY,
    binder_hash text NOT NULL,
    org_id      text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    mode        text NOT NULL CHECK (mode IN ('login', 'link')),
    user_id     text REFERENCES users (id) ON DELETE CASCADE,
    nonce       text NOT NULL,
    verifier    text NOT NULL,
    next        text NOT NULL,
    expires_at  timestamptz NOT NULL
);
CREATE INDEX sso_states_expiry ON sso_states (expires_at);

-- Memberships an organisation's provider created: its sign-ins may change or
-- remove them. Memberships owners added by hand are never in this table.
CREATE TABLE idp_memberships (
    org_id  text NOT NULL,
    user_id text NOT NULL,
    PRIMARY KEY (org_id, user_id),
    FOREIGN KEY (org_id, user_id) REFERENCES memberships (org_id, user_id) ON DELETE CASCADE
);
