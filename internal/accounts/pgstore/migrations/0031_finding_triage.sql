-- Triage of security findings: one organisation's decision (accepted risk,
-- false positive, fixed by a commit or pull request) about one finding of
-- its repository entry, with who, when and why. Findings stay in the shared
-- graph untouched; this only changes the organisation's own report.
CREATE TABLE finding_triage (
    org_id      text NOT NULL,
    graph       text NOT NULL,
    fingerprint text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    rule        text NOT NULL,
    file        text NOT NULL,
    state       text NOT NULL CHECK (state IN ('accepted_risk', 'false_positive', 'fixed_by')),
    reference   text NOT NULL DEFAULT '',
    reason      text NOT NULL,
    actor_id    text NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz,
    PRIMARY KEY (org_id, graph, fingerprint),
    FOREIGN KEY (org_id, graph) REFERENCES repositories (org_id, graph) ON DELETE CASCADE,
    CHECK ((state = 'fixed_by') = (reference <> ''))
);
