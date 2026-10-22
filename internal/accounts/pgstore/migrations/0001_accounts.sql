-- Accounts: users, organisations (tenants), memberships, sessions, API keys
-- and the audit log. Secrets are stored only as hashes.

CREATE TABLE users (
    id             text PRIMARY KEY,
    email          text NOT NULL UNIQUE,
    name           text NOT NULL,
    password_hash  text NOT NULL DEFAULT '',
    platform_admin boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL
);

CREATE TABLE orgs (
    id         text PRIMARY KEY,
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE memberships (
    org_id   text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id  text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role     text NOT NULL CHECK (role IN ('viewer', 'member', 'admin', 'owner')),
    added_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE sessions (
    token_hash text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

CREATE TABLE api_keys (
    id           text PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id       text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name         text NOT NULL,
    prefix       text NOT NULL,
    hash         text NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user ON api_keys (user_id);

-- The audit log is append-only and hash-chained: hash covers the entry and
-- prev_hash, the previous entry's hash. audit_head holds the chain's tip and
-- is locked while appending so concurrent writers cannot fork the chain.
CREATE TABLE events (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL,
    request_id  text NOT NULL DEFAULT '',
    user_id     text NOT NULL DEFAULT '',
    actor_email text NOT NULL DEFAULT '',
    via         text NOT NULL DEFAULT '',
    key_id      text NOT NULL DEFAULT '',
    org_id      text NOT NULL DEFAULT '',
    ip          text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT '',
    action      text NOT NULL,
    target      text NOT NULL DEFAULT '',
    outcome     text NOT NULL,
    status      int NOT NULL DEFAULT 0,
    detail      text NOT NULL DEFAULT '',
    prev_hash   text NOT NULL,
    hash        text NOT NULL
);
CREATE INDEX events_org_at ON events (org_id, at DESC);
CREATE INDEX events_at ON events (at);

CREATE TABLE audit_head (
    id   int PRIMARY KEY CHECK (id = 1),
    hash text NOT NULL
);
INSERT INTO audit_head (id, hash) VALUES (1, '');
