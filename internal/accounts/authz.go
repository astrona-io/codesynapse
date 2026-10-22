package accounts

import (
	"context"
	"fmt"
	"strings"
)

// Relationship-based access control (ReBAC), Zanzibar style.
//
// Schema (fixed here; nothing else may be written):
//
//	organisation  owner > admin > member > viewer > guest
//	              from memberships (users) and from team#member tuples
//	team          maintainer > member                    (users of the team's organisation)
//	repository    admin > editor > viewer                (users and team#member, any organisation)
//
// Computed relations:
//   - An organisation role flows to every repository of the organisation:
//     owner and admin give admin, member gives editor, viewer gives viewer,
//     guest gives nothing.
//   - Organisation owners and admins maintain its teams.
//   - A repository grant to a team reaches every member of the team.
//   - The strongest path wins.
//
// A repository object is one organisation's entry for a graph
// ("<org>|<graph>"): grants are made by that organisation and carry its
// content (reviews, memory) to whoever receives them.

// Object types and relations.
const (
	TypeUser         = "user"
	TypeTeam         = "team"
	TypeOrganisation = "organisation"
	TypeRepository   = "repository"

	RelMember     = "member"
	RelMaintainer = "maintainer"
	RepoAdmin     = "admin"
	RepoEditor    = "editor"
	RepoViewer    = "viewer"

	// MaxTuples bounds one read.
	MaxTuples = 5000
)

// RepoObject is the repository object ID of org's entry for graph.
func RepoObject(orgID, graph string) string { return orgID + "|" + graph }

// SplitRepoObject undoes RepoObject.
func SplitRepoObject(id string) (orgID, graph string, ok bool) { return strings.Cut(id, "|") }

// repoRelationRole maps repository relations to the role names routes use.
var repoRelationRole = map[string]Role{RepoAdmin: Admin, RepoEditor: Member, RepoViewer: Viewer}

// RepoRelation is the relation granting r on a repository ("" for none).
func RepoRelation(r Role) string {
	for rel, role := range repoRelationRole {
		if role == r {
			return rel
		}
	}
	return ""
}

// inherited is what an organisation role gives on its repositories.
func inherited(r Role) Role {
	switch r {
	case Owner, Admin:
		return Admin
	case Member:
		return Member
	case Viewer:
		return Viewer
	}
	return "" // guest: only explicit grants
}

// RepoAccess is a user's access to one organisation's entry for a graph.
type RepoAccess struct {
	OrgID string `json:"org_id"`
	Graph string `json:"graph"`
	Role  Role   `json:"role"` // Admin, Member (editor) or Viewer
	// Via explains the strongest path: "organisation", "user" or "team".
	Via string `json:"via"`
}

// Access is everything a user can reach.
type Access struct {
	UserID string
	// Orgs are the user's effective organisation roles (memberships and
	// team roles), guests included.
	Orgs map[string]Role
	// Teams the user belongs to, by ID (maintainers included).
	Teams map[string]string // team ID -> member or maintainer
	// Repos holds one entry per organisation entry the user reaches.
	Repos []RepoAccess
}

// OrgRole is the user's effective role in orgID ("" without one).
func (a Access) OrgRole(orgID string) Role { return a.Orgs[orgID] }

// AccessOf computes a user's access in a fixed number of reads: memberships,
// the user's team tuples, the teams' organisation roles, repository grants
// to the user and the teams, and the repositories of organisations whose
// role flows down. Entries that lost private access are left out.
func (s *Service) AccessOf(ctx context.Context, userID string) (Access, error) {
	a := Access{UserID: userID, Orgs: map[string]Role{}, Teams: map[string]string{}}
	ms, err := s.St.UserMemberships(ctx, userID)
	if err != nil {
		return a, err
	}
	for _, m := range ms {
		a.Orgs[m.Org.ID] = Max(a.Orgs[m.Org.ID], m.Role)
	}
	teamTuples, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return a, err
	}
	teamIDs := []string{}
	for _, t := range teamTuples {
		if a.Teams[t.ObjectID] != RelMaintainer {
			a.Teams[t.ObjectID] = t.Relation
		}
		teamIDs = append(teamIDs, t.ObjectID)
	}
	var viaTeams []Tuple
	if len(teamIDs) > 0 {
		viaTeams, err = s.St.ReadTuples(ctx, TupleFilter{SubjectType: TypeTeam, SubjectIDs: teamIDs, SubjectRelation: RelMember})
		if err != nil {
			return a, err
		}
	}
	direct, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return a, err
	}
	// Team roles in organisations first: they change what flows down.
	for _, t := range viaTeams {
		if t.ObjectType == TypeOrganisation {
			if r := Role(t.Relation); r.Valid() {
				a.Orgs[t.ObjectID] = Max(a.Orgs[t.ObjectID], r)
			}
		}
	}
	best := map[string]RepoAccess{} // repository object -> strongest
	grant := func(orgID, graph string, r Role, via string) {
		if !r.Valid() {
			return
		}
		k := RepoObject(orgID, graph)
		if cur, ok := best[k]; !ok || Max(cur.Role, r) != cur.Role {
			best[k] = RepoAccess{OrgID: orgID, Graph: graph, Role: r, Via: via}
		}
	}
	for orgID, role := range a.Orgs {
		r := inherited(role)
		if r == "" {
			continue
		}
		repos, err := s.St.OrgRepositories(ctx, orgID)
		if err != nil {
			return a, err
		}
		for _, repo := range repos {
			if repo.Usable() {
				grant(orgID, repo.Graph, r, TypeOrganisation)
			}
		}
	}
	explicit := map[string]Tuple{}
	for _, t := range direct {
		explicit[t.ObjectID+"#"+t.Relation+"@user"] = t
	}
	for _, t := range viaTeams {
		if t.ObjectType == TypeRepository {
			explicit[t.ObjectID+"#"+t.Relation+"@team:"+t.SubjectID] = t
		}
	}
	usable := map[string]map[string]bool{} // org -> graph -> usable
	for _, t := range explicit {
		orgID, graph, ok := SplitRepoObject(t.ObjectID)
		if !ok {
			continue
		}
		if usable[orgID] == nil {
			usable[orgID] = map[string]bool{}
			repos, err := s.St.OrgRepositories(ctx, orgID)
			if err != nil {
				return a, err
			}
			for _, repo := range repos {
				usable[orgID][repo.Graph] = repo.Usable()
			}
		}
		if !usable[orgID][graph] {
			continue // unlinked, or the owning organisation lost access
		}
		via := TypeUser
		if t.SubjectType == TypeTeam {
			via = TypeTeam
		}
		grant(orgID, graph, repoRelationRole[t.Relation], via)
	}
	for _, r := range best {
		a.Repos = append(a.Repos, r)
	}
	return a, nil
}

// validRepoTuple checks a repository grant against the schema.
func validRepoTuple(t Tuple) error {
	if t.ObjectType != TypeRepository || repoRelationRole[t.Relation] == "" {
		return fmt.Errorf("%w: repository relations are admin, editor and viewer", ErrInvalidInput)
	}
	switch {
	case t.SubjectType == TypeUser && t.SubjectRelation == "":
	case t.SubjectType == TypeTeam && t.SubjectRelation == RelMember:
	default:
		return fmt.Errorf("%w: grant to a user or to a team's members", ErrInvalidInput)
	}
	return nil
}

// Reach is one graph in a caller's scope.
type Reach struct {
	Role Role
	// OrgID is the organisation whose content (reviews, memory) the graph
	// shows: a selected organisation's entry first, else the one that
	// shared it.
	OrgID string
	// Ambiguous: several selected organisations use the graph, so writes
	// must say which (X-CodeSynapse-Org).
	Ambiguous bool
}

// Scope narrows Access to the organisations a request acts in: their
// entries, plus entries shared from organisations the user is not a member
// of (sharing works in every context).
func (a Access) Scope(selected map[string]bool) map[string]Reach {
	out := map[string]Reach{}
	for _, r := range a.Repos {
		_, member := a.Orgs[r.OrgID]
		if !selected[r.OrgID] && member {
			continue // another of the user's organisations, not chosen now
		}
		cur, seen := out[r.Graph]
		switch {
		case !seen:
			out[r.Graph] = Reach{Role: r.Role, OrgID: r.OrgID}
		case selected[r.OrgID] && selected[cur.OrgID]:
			cur.Ambiguous = true
			cur.Role = Max(cur.Role, r.Role)
			out[r.Graph] = cur
		case selected[r.OrgID]: // own content beats a share
			out[r.Graph] = Reach{Role: Max(cur.Role, r.Role), OrgID: r.OrgID}
		default:
			cur.Role = Max(cur.Role, r.Role)
			out[r.Graph] = cur
		}
	}
	return out
}
