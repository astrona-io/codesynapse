package nebulastore

import (
	"context"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

var _ store.AccessStore = (*Store)(nil)

// accessEdges are the edge types of access projections.
var accessEdges = []string{"USER_MEMBER_OF_ORG", "USER_MEMBER_OF_TEAM", "TEAM_BELONGS_TO_ORG", "REPO_BELONGS_TO_ORG", "USER_HAS_ACCESS", "TEAM_HAS_ACCESS"}

// accessCols are the properties each access kind sets beside the common ones.
var accessCols = map[model.NodeKind][]string{
	model.KindOrganization:     {"default_repo_access"},
	model.KindUser:             {"user_id", "active", "inherits_default"},
	model.KindTeam:             {"team_id"},
	model.KindAccessRepository: {"repo_id", "graph_id", "usable"},
	"AccessHead":               {"digest"},
}

func accessVID(aid string) string { return vid("a", aid) }

// ReplaceAccess publishes a revision only after every vertex and edge is
// stored: the head vertex is written last, and a reader trusts a revision
// only through its head. Older revisions are removed afterwards.
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
		if err := s.putAccessVertices(ctx, kind, rows); err != nil {
			return err
		}
	}
	if err := s.putAccessEdges(ctx, edges); err != nil {
		return err
	}
	head := model.AccessVertexID(snapshot.TenantID, snapshot.Revision, digest, "AccessHead", snapshot.TenantID)
	if err := s.putAccessVertices(ctx, "AccessHead", []map[string]any{{"aid": head, "tenant_id": snapshot.TenantID, "policy_revision": snapshot.Revision, "digest": digest}}); err != nil {
		return err
	}
	if current, published, err := s.accessHead(ctx, snapshot.TenantID); err != nil {
		return err
	} else if current == snapshot.Revision && published != digest {
		return fmt.Errorf("conflicting published access policies")
	}
	return s.pruneAccess(ctx, snapshot.TenantID, snapshot.Revision)
}

func (s *Store) putAccessVertices(ctx context.Context, kind model.NodeKind, rows []map[string]any) error {
	cols := append([]string{"aid", "kind", "tenant_id", "policy_revision"}, accessCols[kind]...)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quote(c)
	}
	for off := 0; off < len(rows); off += vertexBatch {
		chunk := rows[off:min(off+vertexBatch, len(rows))]
		params := map[string]any{"kind": string(kind)}
		values := make([]string, len(chunk))
		for i, r := range chunk {
			ph := make([]string, len(cols))
			for j, c := range cols {
				name := fmt.Sprintf("v%d_%d", i, j)
				ph[j] = "$" + name
				if c == "kind" {
					params[name] = string(kind)
				} else {
					params[name] = r[c]
				}
			}
			aid, _ := r["aid"].(string)
			values[i] = literal(accessVID(aid)) + ":(" + strings.Join(ph, ",") + ")"
		}
		if _, err := s.write(ctx, "INSERT VERTEX `access_vertex` ("+strings.Join(names, ",")+") VALUES "+strings.Join(values, ","), params); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) putAccessEdges(ctx context.Context, edges []map[string]any) error {
	byType := map[string][]map[string]any{}
	for _, e := range edges {
		t, _ := e["physical"].(string)
		byType[t] = append(byType[t], e)
	}
	for _, t := range accessEdges {
		list := byType[t]
		for off := 0; off < len(list); off += edgeBatch {
			chunk := list[off:min(off+edgeBatch, len(list))]
			params := map[string]any{}
			values := make([]string, len(chunk))
			for i, e := range chunk {
				from, _ := e["from"].(string)
				to, _ := e["to"].(string)
				ten, role, rank := fmt.Sprintf("t%d", i), fmt.Sprintf("r%d", i), fmt.Sprintf("k%d", i)
				params[ten], params[role], params[rank] = e["tenant_id"], e["role"], e["rank"]
				values[i] = literal(accessVID(from)) + "->" + literal(accessVID(to)) + "@0:($" + ten + ",$" + role + ",$" + rank + ")"
			}
			if _, err := s.write(ctx, "INSERT EDGE "+quote(t)+" (`tenant_id`,`role`,`rank`) VALUES "+strings.Join(values, ","), params); err != nil {
				return err
			}
		}
	}
	return nil
}

// pruneAccess removes the tenant's revisions older than current.
func (s *Store) pruneAccess(ctx context.Context, tenant string, current int64) error {
	for {
		rows, err := s.read(ctx, "MATCH (v:`access_vertex`) WHERE v.`access_vertex`.`tenant_id` == $tenant AND v.`access_vertex`.`policy_revision` < $rev RETURN id(v) AS id LIMIT 400",
			map[string]any{"tenant": tenant, "rev": current})
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i], _ = r["id"].(string)
		}
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(ids)+" WITH EDGE", nil); err != nil {
			return err
		}
	}
}

// accessHead returns the newest published revision and its digest; two heads
// of one revision (a conflict) answer no digest.
func (s *Store) accessHead(ctx context.Context, tenant string) (int64, string, error) {
	rows, err := s.read(ctx, "MATCH (v:`access_vertex`) WHERE v.`access_vertex`.`tenant_id` == $tenant AND v.`access_vertex`.`kind` == \"AccessHead\""+
		" RETURN v.`access_vertex`.`policy_revision` AS rev, v.`access_vertex`.`digest` AS digest ORDER BY rev DESC LIMIT 2", map[string]any{"tenant": tenant})
	if err != nil || len(rows) == 0 {
		return 0, "", err
	}
	rev, _ := rows[0]["rev"].(int64)
	if len(rows) > 1 && rows[1]["rev"] == rows[0]["rev"] {
		return rev, "", nil
	}
	digest, _ := rows[0]["digest"].(string)
	return rev, digest, nil
}

// accessEdge reads one edge's role and rank; ok is false when it is absent.
func (s *Store) accessEdge(ctx context.Context, typ, from, to, tenant string) (role string, rank int64, ok bool, err error) {
	rows, err := s.read(ctx, "FETCH PROP ON "+quote(typ)+" "+literal(accessVID(from))+"->"+literal(accessVID(to))+"@0 YIELD properties(edge).tenant_id AS tenant, properties(edge).role AS role, properties(edge).rank AS rank", nil)
	if err != nil || len(rows) == 0 || rows[0]["tenant"] != tenant {
		return "", 0, false, err
	}
	role, _ = rows[0]["role"].(string)
	rank, _ = rows[0]["rank"].(int64)
	return role, rank, true, nil
}

// grantRank is the rank of a user or team grant, or false when the edge is
// absent or its role and rank disagree (a damaged policy grants nothing).
func (s *Store) grantRank(ctx context.Context, typ, from, to, tenant string, allowNone bool) (rank int, found, valid bool, err error) {
	role, r, ok, err := s.accessEdge(ctx, typ, from, to, tenant)
	if err != nil || !ok {
		return 0, false, true, err
	}
	want, known := map[string]int{"NONE": 0, "READ": 1, "WRITE": 2, "ADMIN": 3}[role]
	if !known || (!allowNone && want == 0) || int64(want) != r {
		return 0, true, false, nil
	}
	return want, true, true, nil
}

// vertexProps reads access vertices of the tenant, keyed by aid.
func (s *Store) accessVertex(ctx context.Context, aid, tenant string) (map[string]any, error) {
	rows, err := s.read(ctx, "FETCH PROP ON `access_vertex` "+literal(accessVID(aid))+" YIELD properties(vertex) AS p", nil)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	p, _ := rows[0]["p"].(map[string]any)
	if p["tenant_id"] != tenant {
		return nil, nil
	}
	return p, nil
}

// AccessAt implements store.AccessStore with constant, tenant-rooted
// statements over IDs derived from the published head; a revision or digest
// that is not the head's answers no permission.
func (s *Store) AccessAt(ctx context.Context, tenant string, revision int64, userID, repoID string) (model.AccessDecision, error) {
	none := model.AccessDecision{}
	if tenant == "" || revision < 1 || userID == "" || repoID == "" {
		return none, nil
	}
	current, digest, err := s.accessHead(ctx, tenant)
	if err != nil {
		return none, err
	}
	if current != revision || digest == "" {
		return none, nil
	}
	org := model.AccessVertexID(tenant, revision, digest, model.KindOrganization, tenant)
	user := model.AccessVertexID(tenant, revision, digest, model.KindUser, userID)
	repo := model.AccessVertexID(tenant, revision, digest, model.KindAccessRepository, repoID)
	o, err := s.accessVertex(ctx, org, tenant)
	if err != nil || o == nil {
		return none, err
	}
	u, err := s.accessVertex(ctx, user, tenant)
	if err != nil || u == nil || u["active"] != true {
		return none, err
	}
	r, err := s.accessVertex(ctx, repo, tenant)
	if err != nil || r == nil || r["usable"] != true {
		return none, err
	}
	for _, e := range [][3]string{{"USER_MEMBER_OF_ORG", user, org}, {"REPO_BELONGS_TO_ORG", repo, org}} {
		if _, _, ok, err := s.accessEdge(ctx, e[0], e[1], e[2], tenant); err != nil || !ok {
			return none, err
		}
	}
	direct, hasDirect, valid, err := s.grantRank(ctx, "USER_HAS_ACCESS", user, repo, tenant, true)
	if err != nil || !valid {
		return none, err
	}
	rows, err := s.read(ctx, "GO FROM "+literal(accessVID(user))+" OVER `USER_MEMBER_OF_TEAM` YIELD dst(edge) AS team | LIMIT "+fmt.Sprint(model.MaxAccessTeams+1), nil)
	if err != nil {
		return none, err
	}
	if len(rows) > model.MaxAccessTeams {
		return none, nil
	}
	best, granted := 0, false
	for _, row := range rows {
		team, _ := row["team"].(string)
		// The team must belong to the organisation of this revision.
		tp, err := s.read(ctx, "FETCH PROP ON `access_vertex` "+literal(team)+" YIELD properties(vertex).aid AS aid, properties(vertex).kind AS kind, properties(vertex).tenant_id AS tenant", nil)
		if err != nil {
			return none, err
		}
		if len(tp) != 1 || tp[0]["kind"] != string(model.KindTeam) || tp[0]["tenant"] != tenant {
			continue
		}
		teamAID, _ := tp[0]["aid"].(string)
		if _, _, ok, err := s.accessEdge(ctx, "TEAM_BELONGS_TO_ORG", teamAID, org, tenant); err != nil {
			return none, err
		} else if !ok {
			continue
		}
		rank, found, valid, err := s.grantRank(ctx, "TEAM_HAS_ACCESS", teamAID, repo, tenant, false)
		if err != nil || !valid {
			return none, err
		}
		if found {
			best, granted = max(best, rank), true
		}
	}
	fallback := 0
	if u["inherits_default"] == true && o["default_repo_access"] == "READ_ONLY" {
		fallback = 1
	}
	rank := fallback
	switch {
	case hasDirect:
		rank = direct
	case granted:
		rank = best
	}
	graph, _ := r["graph_id"].(string)
	return model.AccessDecision{GraphID: graph, Rank: rank}, nil
}
