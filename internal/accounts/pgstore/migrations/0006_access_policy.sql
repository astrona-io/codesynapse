ALTER TABLE orgs ADD COLUMN default_repo_access text NOT NULL DEFAULT 'NONE'
 CHECK (default_repo_access IN ('NONE', 'READ_ONLY'));
ALTER TABLE orgs ADD COLUMN policy_revision bigint NOT NULL DEFAULT 1;
-- Preserve an explicit deny for contradictory user grants. Invalid team grants
-- fail closed in the resolver until an administrator replaces them.
WITH conflicting AS (
 SELECT object_id,subject_type,subject_id,subject_relation
 FROM relation_tuples WHERE object_type='repository'
 GROUP BY object_id,subject_type,subject_id,subject_relation HAVING count(*)>1
), removed AS (
 DELETE FROM relation_tuples t USING conflicting c
 WHERE t.object_type='repository' AND t.object_id=c.object_id
 AND t.subject_type=c.subject_type AND t.subject_id=c.subject_id
 AND t.subject_relation=c.subject_relation RETURNING t.*
)
INSERT INTO relation_tuples(object_type,object_id,relation,subject_type,subject_id,subject_relation,created_by,created_at)
SELECT 'repository',object_id,CASE WHEN subject_type='user' THEN 'none' ELSE 'invalid' END,
 subject_type,subject_id,subject_relation,'',min(created_at)
FROM removed GROUP BY object_id,subject_type,subject_id,subject_relation;
CREATE UNIQUE INDEX repository_subject_override ON relation_tuples
 (object_id, subject_type, subject_id, subject_relation) WHERE object_type = 'repository';
ALTER TABLE teams ADD CONSTRAINT teams_org_id_unique UNIQUE(org_id,id);
ALTER TABLE relation_tuples ADD COLUMN repo_org_id text GENERATED ALWAYS AS
 (CASE WHEN object_type='repository' THEN split_part(object_id,'|',1) END) STORED;
ALTER TABLE relation_tuples ADD COLUMN repo_graph text GENERATED ALWAYS AS
 (CASE WHEN object_type='repository' THEN substring(object_id from position('|' in object_id)+1) END) STORED;
ALTER TABLE relation_tuples ADD COLUMN repo_user_id text GENERATED ALWAYS AS
 (CASE WHEN object_type='repository' AND subject_type='user' THEN subject_id END) STORED;
ALTER TABLE relation_tuples ADD COLUMN repo_team_id text GENERATED ALWAYS AS
 (CASE WHEN object_type='repository' AND subject_type='team' THEN subject_id END) STORED;
-- NOT VALID preserves legacy external shares for the access-change report.
-- New writes must satisfy tenant ownership and explicit membership.
ALTER TABLE relation_tuples ADD CONSTRAINT grant_repository_fk FOREIGN KEY(repo_org_id,repo_graph)
 REFERENCES repositories(org_id,graph) ON DELETE CASCADE NOT VALID;
ALTER TABLE relation_tuples ADD CONSTRAINT grant_user_membership_fk FOREIGN KEY(repo_org_id,repo_user_id)
 REFERENCES memberships(org_id,user_id) ON DELETE CASCADE NOT VALID;
ALTER TABLE relation_tuples ADD CONSTRAINT grant_team_org_fk FOREIGN KEY(repo_org_id,repo_team_id)
 REFERENCES teams(org_id,id) ON DELETE CASCADE NOT VALID;
CREATE TABLE access_policy_outbox (
 org_id text PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
 revision bigint NOT NULL
);
INSERT INTO access_policy_outbox SELECT id, policy_revision FROM orgs;
