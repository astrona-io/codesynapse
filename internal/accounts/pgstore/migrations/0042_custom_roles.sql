-- Custom roles and scoped API keys (docs/plans/OPENFGA_PLAN.md, phase 3).
-- A custom role is a named bundle of permissions of one resource; it is
-- granted through relation_tuples with the relation 'role:<id>'.
CREATE TABLE custom_roles (
    id          text PRIMARY KEY,
    org_id      text NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 64),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 500),
    resource    text NOT NULL CHECK (resource IN ('repository', 'organisation')),
    permissions text[] NOT NULL CHECK (cardinality(permissions) BETWEEN 1 AND 64),
    created_by  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE UNIQUE INDEX custom_roles_name ON custom_roles (org_id, lower(name));

-- NULL: an unscoped key, which may do whatever its user may.
ALTER TABLE api_keys ADD COLUMN scope text[];
