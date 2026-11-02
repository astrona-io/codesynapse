-- Role and relation names become read, write and full (docs/plans/OPENFGA_PLAN.md,
-- phase 2): organisation roles guest/read/write/full/owner, repository relations
-- read/write/full/deny and team relations read/full (team#read as a subject).
-- Nobody's access changes, so policy revisions are not bumped. Audit entries
-- keep the names they were written with.

ALTER TABLE memberships DROP CONSTRAINT IF EXISTS memberships_role_check;
UPDATE memberships SET role = CASE role WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' END
 WHERE role IN ('viewer', 'member', 'admin');
ALTER TABLE memberships ADD CONSTRAINT memberships_role_check CHECK (role IN ('guest', 'read', 'write', 'full', 'owner'));

-- Repository grants.
UPDATE relation_tuples SET relation = CASE relation WHEN 'viewer' THEN 'read' WHEN 'editor' THEN 'write' WHEN 'admin' THEN 'full' WHEN 'none' THEN 'deny' END
 WHERE object_type = 'repository' AND relation IN ('viewer', 'editor', 'admin', 'none');
-- Organisation roles granted to teams.
UPDATE relation_tuples SET relation = CASE relation WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' END
 WHERE object_type = 'organisation' AND relation IN ('viewer', 'member', 'admin');
-- Team membership.
UPDATE relation_tuples SET relation = CASE relation WHEN 'member' THEN 'read' WHEN 'maintainer' THEN 'full' END
 WHERE object_type = 'team' AND relation IN ('member', 'maintainer');
UPDATE relation_tuples SET subject_relation = 'read' WHERE subject_type = 'team' AND subject_relation = 'member';

-- Jobs carry the repository or organisation role they were queued with.
UPDATE jobs SET role = CASE role WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'editor' THEN 'write' WHEN 'admin' THEN 'full' WHEN 'none' THEN 'deny' END
 WHERE role IN ('viewer', 'member', 'editor', 'admin', 'none');

UPDATE org_invitations SET role = CASE role WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' END
 WHERE role IN ('viewer', 'member', 'admin');

-- Single sign-on: the default role and the roles groups map to.
UPDATE identity_providers SET settings = jsonb_set(settings, '{default_role}',
  to_jsonb(CASE settings->>'default_role' WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' END))
 WHERE settings->>'default_role' IN ('viewer', 'member', 'admin');
UPDATE identity_providers SET settings = jsonb_set(settings, '{role_groups}',
  (SELECT jsonb_object_agg(g.key, CASE g.value WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' ELSE g.value END)
     FROM jsonb_each_text(settings->'role_groups') AS g))
 WHERE jsonb_typeof(settings->'role_groups') = 'object' AND settings->'role_groups' <> '{}'::jsonb;

-- Daily statistics count memberships per role.
UPDATE platform_stats_daily SET dimension = CASE dimension WHEN 'viewer' THEN 'read' WHEN 'member' THEN 'write' WHEN 'admin' THEN 'full' END
 WHERE metric = 'memberships' AND dimension IN ('viewer', 'member', 'admin');
