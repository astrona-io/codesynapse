-- Organisations (or users) on a git host whose new public repositories are
-- added to a CodeSynapse organisation automatically. The watch runs as the
-- member who created it; "known" is the repositories already seen, so one a
-- person removed is not added again.
CREATE TABLE org_watches (
    org_id           text NOT NULL,
    key              text NOT NULL,
    actor_id         text NOT NULL,
    execution        jsonb NOT NULL,
    provider         text NOT NULL,
    host             text NOT NULL,
    login            text NOT NULL,
    include_archived boolean NOT NULL DEFAULT false,
    include_forks    boolean NOT NULL DEFAULT false,
    paused           boolean NOT NULL DEFAULT false,
    last_scan        timestamptz,
    last_status      text NOT NULL DEFAULT '',
    last_error       text NOT NULL DEFAULT '',
    next_scan        timestamptz NOT NULL DEFAULT now(),
    added            integer NOT NULL DEFAULT 0,
    known            jsonb NOT NULL DEFAULT '[]',
    PRIMARY KEY (org_id, key)
);
CREATE INDEX org_watches_due ON org_watches (next_scan) WHERE NOT paused;
