-- Accounting identities survive unlinking and report retention. Reusing a
-- repository entry must not reset its cumulative stream or run tombstones.
CREATE TABLE ingest_metric_stream (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    stream_id text NOT NULL UNIQUE,
    started_at timestamptz NOT NULL,
    sample_at timestamptz NOT NULL,
    last_exported_at timestamptz NOT NULL,
    dropped_series bigint NOT NULL DEFAULT 0 CHECK (dropped_series >= 0)
);
CREATE TABLE ingest_repository_totals (
    org_id text NOT NULL,
    graph text NOT NULL,
    repository_id text NOT NULL UNIQUE,
    admitted boolean NOT NULL,
    started_at timestamptz NOT NULL,
    scanned bigint NOT NULL DEFAULT 0 CHECK (scanned >= 0),
    written bigint NOT NULL DEFAULT 0 CHECK (written >= 0),
    ignored_path bigint NOT NULL DEFAULT 0 CHECK (ignored_path >= 0),
    generated_code bigint NOT NULL DEFAULT 0 CHECK (generated_code >= 0),
    binary_files bigint NOT NULL DEFAULT 0 CHECK (binary_files >= 0),
    non_k8s_yaml bigint NOT NULL DEFAULT 0 CHECK (non_k8s_yaml >= 0),
    latest_success text NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, graph)
);
CREATE TABLE ingest_runs (
    org_id text NOT NULL,
    graph text NOT NULL,
    run_id text NOT NULL,
    identity_json jsonb NOT NULL,
    report_json jsonb,
    report_hash text NOT NULL,
    status text NOT NULL,
    graph_commit text NOT NULL,
    started_at timestamptz NOT NULL,
    terminal boolean NOT NULL DEFAULT false,
    PRIMARY KEY (org_id, graph, run_id),
    FOREIGN KEY (org_id, graph) REFERENCES ingest_repository_totals(org_id, graph)
);
CREATE INDEX ingest_runs_history ON ingest_runs(org_id, graph, started_at DESC, run_id DESC) WHERE report_json IS NOT NULL;
CREATE INDEX ingest_runs_pending ON ingest_runs(started_at, graph) WHERE NOT terminal;
CREATE UNIQUE INDEX ingest_runs_one_pending ON ingest_runs(org_id, graph) WHERE NOT terminal;
