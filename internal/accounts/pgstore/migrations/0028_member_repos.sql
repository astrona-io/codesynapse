-- A repository entry belongs to the organisation (owner_id empty) or to the
-- member who added it to their own space. Personal entries get no
-- organisation default access: only the owner and whom they grant see them.
ALTER TABLE repositories ADD COLUMN owner_id text NOT NULL DEFAULT '';

-- Organisations choose whether members may add repositories to their own
-- space. Without a row they may (see MemberReposEnabled).
CREATE TABLE org_member_repos (
    org_id     text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    enabled    boolean NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now()
);
