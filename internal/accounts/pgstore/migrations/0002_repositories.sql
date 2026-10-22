-- Organisation-owned repository entries. A graph (the Neo4j repository name)
-- may be referenced by several organisations: public repositories are
-- indexed once and shared, private ones only after an organisation proved
-- access. Deleting the last reference deletes the graph.
CREATE TABLE repositories (
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    graph      text NOT NULL,
    remote     text NOT NULL DEFAULT '',
    visibility text NOT NULL CHECK (visibility IN ('public', 'private', 'local')),
    added_by   text NOT NULL DEFAULT '',
    added_at   timestamptz NOT NULL,
    PRIMARY KEY (org_id, graph)
);
CREATE INDEX repositories_graph ON repositories (graph);
