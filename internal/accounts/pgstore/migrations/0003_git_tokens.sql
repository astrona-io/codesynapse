-- Organisation Git tokens, sealed with AES-256-GCM by the API (the database
-- never sees a token in clear). key_id names the sealing key.
CREATE TABLE org_git_tokens (
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    host       text NOT NULL,
    preview    text NOT NULL,
    key_id     text NOT NULL,
    sealed     bytea NOT NULL,
    created_by text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, host)
);

-- Private repositories are re-checked with the organisation's own token;
-- an organisation whose token no longer reads the remote loses the graph
-- until a working token restores it.
ALTER TABLE repositories
    ADD COLUMN access text NOT NULL DEFAULT 'ok' CHECK (access IN ('ok', 'lost')),
    ADD COLUMN verified_at timestamptz;
