-- Relationship-based access control: teams and Zanzibar-style relation
-- tuples (object#relation@subject). Organisation memberships stay in
-- memberships; "guest" members inherit no repository.
ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_role_check;
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check CHECK (role IN ('guest', 'viewer', 'member', 'admin', 'owner'));

CREATE TABLE teams (
    id         text PRIMARY KEY,
    org_id     text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    slug       text NOT NULL,
    name       text NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE relation_tuples (
    object_type      text NOT NULL,
    object_id        text NOT NULL,
    relation         text NOT NULL,
    subject_type     text NOT NULL,
    subject_id       text NOT NULL,
    subject_relation text NOT NULL DEFAULT '',
    created_by       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL,
    PRIMARY KEY (object_type, object_id, relation, subject_type, subject_id, subject_relation)
);
-- "What can this subject reach?" (listing a user's teams and grants).
CREATE INDEX relation_tuples_subject ON relation_tuples (subject_type, subject_id, subject_relation);
