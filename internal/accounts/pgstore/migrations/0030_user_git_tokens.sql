-- A person's own Git tokens, for the repositories in their own space. Sealed
-- by the API (bound to the person and host, never usable as an organisation
-- token); the database never sees a token in clear.
CREATE TABLE user_git_tokens (
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    host       text NOT NULL,
    preview    text NOT NULL,
    key_id     text NOT NULL,
    sealed     bytea NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, host)
);
