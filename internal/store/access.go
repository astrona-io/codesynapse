package store

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/model"
)

// AccessStore holds rebuildable explanations. Accounts remain the authority.
type AccessStore interface {
	ReplaceAccess(ctx context.Context, snapshot model.AccessSnapshot) error
	// AccessAt returns no permission for absent, incomplete or stale revisions.
	AccessAt(ctx context.Context, tenant string, revision int64, userID, repoID string) (model.AccessDecision, error)
}

// AccessRows translates a validated snapshot to parameter maps for both adapters.
// The revision is part of every ID; no access vertex is a code-graph Node.
func AccessRows(s model.AccessSnapshot) (map[model.NodeKind][]map[string]any, []map[string]any) {
	nodes := map[model.NodeKind][]map[string]any{}
	edges := []map[string]any{}
	digest := s.Digest()
	id := func(kind model.NodeKind, key string) string {
		return model.AccessVertexID(s.TenantID, s.Revision, digest, kind, key)
	}
	add := func(kind model.NodeKind, key string, props map[string]any) {
		props["tenant_id"], props["policy_revision"], props["aid"] = s.TenantID, s.Revision, id(kind, key)
		nodes[kind] = append(nodes[kind], props)
	}
	edge := func(fromKind model.NodeKind, from string, toKind model.NodeKind, to, logical, physical, role string) {
		edges = append(edges, map[string]any{"from": id(fromKind, from), "to": id(toKind, to), "logical": logical,
			"physical": physical, "tenant_id": s.TenantID, "role": role, "rank": model.AccessRank(role)})
	}
	add(model.KindOrganization, s.TenantID, map[string]any{"default_repo_access": s.Default})
	for _, u := range s.Users {
		add(model.KindUser, u.ID, map[string]any{"user_id": u.ID, "active": u.Active, "inherits_default": u.InheritsDefault})
		edge(model.KindUser, u.ID, model.KindOrganization, s.TenantID, "MEMBER_OF", "USER_MEMBER_OF_ORG", "")
	}
	for _, t := range s.Teams {
		add(model.KindTeam, t, map[string]any{"team_id": t})
		edge(model.KindTeam, t, model.KindOrganization, s.TenantID, "BELONGS_TO", "TEAM_BELONGS_TO_ORG", "")
	}
	for _, r := range s.Repos {
		add(model.KindAccessRepository, r.ID, map[string]any{"repo_id": r.ID, "graph_id": r.GraphID, "usable": r.Usable, "personal": r.Personal})
		edge(model.KindAccessRepository, r.ID, model.KindOrganization, s.TenantID, "BELONGS_TO", "REPO_BELONGS_TO_ORG", "")
	}
	for _, m := range s.Members {
		edge(model.KindUser, m.UserID, model.KindTeam, m.TeamID, "MEMBER_OF", "USER_MEMBER_OF_TEAM", "")
	}
	for _, g := range s.Grants {
		physical := "USER_HAS_ACCESS"
		if g.SubjectKind == model.KindTeam {
			physical = "TEAM_HAS_ACCESS"
		}
		edge(g.SubjectKind, g.SubjectID, model.KindAccessRepository, g.RepoID, "HAS_ACCESS", physical, g.Role)
	}
	return nodes, edges
}
