-- The opt-in gateway prompt log (docs/GATEWAY_PLAN.md, "Prompt log"). Off
-- unless an owner or admin turns it on; kept apart from the hash-chained
-- events table, which only holds digests of the same texts. Texts are
-- redacted and capped in the code (accounts.LogText); the checks below bound
-- them again. Rows go with their organisation.
CREATE TABLE org_prompt_log (
    org_id         text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    enabled        boolean NOT NULL DEFAULT false,
    retention_days integer NOT NULL DEFAULT 30 CHECK (retention_days BETWEEN 1 AND 400),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gateway_prompt_log (
    id               bigserial PRIMARY KEY,
    org_id           text NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    at               timestamptz NOT NULL,
    audit_id         bigint NOT NULL DEFAULT 0,
    user_id          text NOT NULL DEFAULT '' CHECK (length(user_id) <= 64),
    key_id           text NOT NULL DEFAULT '' CHECK (length(key_id) <= 64),
    session_id       text NOT NULL DEFAULT '' CHECK (length(session_id) <= 128),
    path             text NOT NULL DEFAULT '' CHECK (length(path) <= 64),
    model            text NOT NULL DEFAULT '' CHECK (length(model) <= 64),
    route            text NOT NULL CHECK (route IN ('local', 'cloud', 'rejected')),
    status           integer NOT NULL DEFAULT 0,
    duration_ms      bigint NOT NULL DEFAULT 0,
    shortened        boolean NOT NULL DEFAULT false,
    fallthrough      text NOT NULL DEFAULT '' CHECK (length(fallthrough) <= 32),
    prompt_sha256    text NOT NULL DEFAULT '' CHECK (prompt_sha256 ~ '^([0-9a-f]{64})?$'),
    prompt_bytes     integer NOT NULL DEFAULT 0,
    forwarded_sha256 text NOT NULL DEFAULT '' CHECK (forwarded_sha256 ~ '^([0-9a-f]{64})?$'),
    forwarded_bytes  integer NOT NULL DEFAULT 0,
    prompt           text NOT NULL DEFAULT '' CHECK (octet_length(prompt) <= 65536),
    forwarded        text NOT NULL DEFAULT '' CHECK (octet_length(forwarded) <= 65536),
    answer           text NOT NULL DEFAULT '' CHECK (octet_length(answer) <= 65536)
);
CREATE INDEX gateway_prompt_log_org_id ON gateway_prompt_log (org_id, id);
CREATE INDEX gateway_prompt_log_org_at ON gateway_prompt_log (org_id, at);

-- Runs of an organisation's oldest rows sealed into archive files before
-- they leave the database (like audit_segments).
CREATE TABLE gateway_prompt_log_segments (
    id        bigserial PRIMARY KEY,
    org_id    text NOT NULL,
    first_id  bigint NOT NULL,
    last_id   bigint NOT NULL,
    first_at  timestamptz NOT NULL,
    last_at   timestamptz NOT NULL,
    count     integer NOT NULL,
    sha256    text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    bytes     bigint NOT NULL,
    backend   text NOT NULL,
    key       text NOT NULL,
    sealed_at timestamptz NOT NULL
);
CREATE INDEX gateway_prompt_log_segments_last_at ON gateway_prompt_log_segments (last_at);
