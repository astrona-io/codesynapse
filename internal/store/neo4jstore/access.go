package neo4jstore

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// ReplaceAccess stages disjoint revision vertices before publishing the head.
func (s *Store) ReplaceAccess(ctx context.Context, snapshot model.AccessSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	digest := snapshot.Digest()
	current, published, err := s.accessHead(ctx, snapshot.TenantID)
	if err != nil {
		return err
	}
	if current > snapshot.Revision {
		return nil
	}
	if current == snapshot.Revision {
		if published != digest {
			return fmt.Errorf("access revision already has a different policy")
		}
		return nil
	}
	nodes, edges := store.AccessRows(snapshot)
	for kind, rows := range nodes {
		if !kind.Valid() {
			return fmt.Errorf("invalid access kind %q", kind)
		}
		q := fmt.Sprintf("UNWIND $rows AS p MERGE (n:AccessVertex {aid:p.aid}) SET n=p SET n:%s", kind)
		if err := s.batched(ctx, q, rows, nil); err != nil {
			return err
		}
	}
	for _, kind := range []model.EdgeKind{model.EdgeMemberOf, model.EdgeBelongsTo, model.EdgeHasAccess} {
		var rows []map[string]any
		for _, e := range edges {
			if e["logical"] == string(kind) {
				rows = append(rows, e)
			}
		}
		q := fmt.Sprintf(`UNWIND $rows AS p MATCH (a:AccessVertex {aid:p.from}), (b:AccessVertex {aid:p.to})
MERGE (a)-[e:%s]->(b) SET e.tenant_id=p.tenant_id, e.role=p.role, e.rank=p.rank`, kind)
		if err := s.batched(ctx, q, rows, nil); err != nil {
			return err
		}
	}
	p := map[string]any{"tenant": snapshot.TenantID, "revision": snapshot.Revision, "digest": digest,
		"head": model.AccessVertexID(snapshot.TenantID, snapshot.Revision, digest, "AccessHead", snapshot.TenantID), "batch": deleteBatch}
	if _, err := s.write(ctx, `MERGE (h:AccessHead {aid:$head})
ON CREATE SET h.tenant_id=$tenant, h.policy_revision=$revision, h.digest=$digest`, p); err != nil {
		return err
	}
	if current, published, err := s.accessHead(ctx, snapshot.TenantID); err != nil {
		return err
	} else if current == snapshot.Revision && published != digest {
		return fmt.Errorf("conflicting published access policies")
	}
	for _, label := range []string{"AccessVertex", "AccessHead"} {
		q := fmt.Sprintf(`MATCH (h:AccessHead {tenant_id:$tenant}) WITH max(h.policy_revision) AS revision
MATCH (n:%s {tenant_id:$tenant}) WHERE n.policy_revision < revision
WITH n LIMIT $batch DETACH DELETE n RETURN count(*) AS c`, label)
		if err := s.drain(ctx, q, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) accessHead(ctx context.Context, tenant string) (int64, string, error) {
	r, err := s.read(ctx, `MATCH (h:AccessHead {tenant_id:$tenant}) WITH max(h.policy_revision) AS revision
MATCH (h:AccessHead {tenant_id:$tenant, policy_revision:revision}) RETURN revision, h.digest AS digest LIMIT 2`, map[string]any{"tenant": tenant})
	if err != nil {
		return 0, "", err
	}
	if len(r.Records) == 0 {
		return 0, "", nil
	}
	if len(r.Records) != 1 {
		return int64(num(r.Records[0], "revision")), "", nil
	}
	return int64(num(r.Records[0], "revision")), str(r.Records[0], "digest"), nil
}

const accessQuery = `MATCH (head:AccessHead {tenant_id:$tenant}) WITH max(head.policy_revision) AS current
MATCH (head:AccessHead {tenant_id:$tenant,policy_revision:current}) WITH collect(head) AS heads
WHERE size(heads)=1 AND heads[0].policy_revision=$revision AND heads[0].digest=$digest
MATCH (o:Organization:AccessVertex {aid:$org,tenant_id:$tenant,policy_revision:$revision})
MATCH (u:User:AccessVertex {aid:$user,tenant_id:$tenant,active:true})-[:MEMBER_OF {tenant_id:$tenant}]->(o)
MATCH (r:AccessRepository:AccessVertex {aid:$repo,tenant_id:$tenant,usable:true})-[:BELONGS_TO {tenant_id:$tenant}]->(o)
OPTIONAL MATCH (u)-[d:HAS_ACCESS {tenant_id:$tenant}]->(r)
WITH o,u,r,collect(d) AS direct
WHERE size(direct)<=1 AND all(x IN direct WHERE x.rank=CASE x.role WHEN 'NONE' THEN 0 WHEN 'READ' THEN 1 WHEN 'WRITE' THEN 2 WHEN 'ADMIN' THEN 3 ELSE -1 END AND x.rank IN [0,1,2,3])
CALL { WITH u,o MATCH (u)-[:MEMBER_OF {tenant_id:$tenant}]->(t:Team {tenant_id:$tenant})-[:BELONGS_TO {tenant_id:$tenant}]->(o)
WITH DISTINCT t LIMIT $probe RETURN collect(t) AS teams }
WITH o,u,r,direct,teams WHERE size(teams)<$probe
CALL { WITH teams,r UNWIND teams AS t OPTIONAL MATCH (t)-[g:HAS_ACCESS {tenant_id:$tenant}]->(r)
WITH t,collect(g) AS grants RETURN collect({grants:grants}) AS groups }
WITH o,u,r,direct,groups WHERE all(t IN groups WHERE size(t.grants)<=1 AND all(x IN t.grants WHERE x.rank IN [1,2,3] AND x.rank=CASE x.role WHEN 'READ' THEN 1 WHEN 'WRITE' THEN 2 WHEN 'ADMIN' THEN 3 ELSE -1 END))
WITH o,u,r,direct,reduce(rs=[],t IN groups | rs+[g IN t.grants | g.rank]) AS ranks
RETURN r.graph_id AS graph_id, CASE WHEN size(direct)=1 THEN direct[0].rank WHEN size(ranks)>0 THEN reduce(best=0,x IN ranks | CASE WHEN x>best THEN x ELSE best END)
WHEN u.inherits_default AND o.default_repo_access='READ_ONLY' THEN 1 ELSE 0 END AS rank`

// AccessAt evaluates only a complete revision matching the authoritative caller.
func (s *Store) AccessAt(ctx context.Context, tenant string, revision int64, userID, repoID string) (model.AccessDecision, error) {
	if tenant == "" || revision < 1 || userID == "" || repoID == "" {
		return model.AccessDecision{}, nil
	}
	current, digest, err := s.accessHead(ctx, tenant)
	if err != nil {
		return model.AccessDecision{}, err
	}
	if current != revision || digest == "" {
		return model.AccessDecision{}, nil
	}
	p := map[string]any{"tenant": tenant, "revision": revision, "digest": digest, "org": model.AccessVertexID(tenant, revision, digest, model.KindOrganization, tenant),
		"user": model.AccessVertexID(tenant, revision, digest, model.KindUser, userID), "repo": model.AccessVertexID(tenant, revision, digest, model.KindAccessRepository, repoID), "probe": model.MaxAccessTeams + 1}
	r, err := s.read(ctx, accessQuery, p)
	if err != nil {
		return model.AccessDecision{}, err
	}
	if len(r.Records) != 1 {
		return model.AccessDecision{}, nil
	}
	return model.AccessDecision{GraphID: str(r.Records[0], "graph_id"), Rank: num(r.Records[0], "rank")}, nil
}
