CREATE TABLE repository_policy_approvals (
    org_id text NOT NULL,
    graph text NOT NULL,
    digest text NOT NULL,
    config_json text NOT NULL,
    reason text NOT NULL,
    actor_id text NOT NULL REFERENCES users(id),
    allow_exclusions boolean NOT NULL,
    allow_suppressions boolean NOT NULL,
    allow_annotation boolean NOT NULL,
    updated_at timestamptz NOT NULL,
    expires_at timestamptz,
    PRIMARY KEY (org_id, graph),
    FOREIGN KEY (org_id, graph) REFERENCES repositories(org_id, graph) ON DELETE CASCADE
);

CREATE TABLE repository_annotations (
    org_id text NOT NULL,
    graph text NOT NULL,
    symbol_id text NOT NULL,
    commit_id text NOT NULL,
    revision text NOT NULL,
    fingerprint text NOT NULL,
    policy_digest text NOT NULL,
    job_id text NOT NULL,
    origin_session_id text NOT NULL REFERENCES execution_sessions(id),
    summary text NOT NULL,
    taxonomy jsonb NOT NULL,
    confidence double precision CHECK (confidence >= 0 AND confidence <= 1),
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, graph, symbol_id),
    FOREIGN KEY (org_id, graph) REFERENCES repositories(org_id, graph) ON DELETE CASCADE
);
