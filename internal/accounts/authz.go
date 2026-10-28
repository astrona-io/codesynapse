package accounts

import (
	"context"
	"fmt"
	"strings"
)

// Repository permissions require explicit organisation membership. Direct user
// overrides take precedence over team grants, then the organisation read default.
// Organisation roles manage policy and membership; they grant no code access.

// Object types and relations.
const (
	TypeUser         = "user"
	TypeTeam         = "team"
	TypeOrganisation = "organisation"
	TypeRepository   = "repository"

	RelMember     = "member"
	RelMaintainer = "maintainer"
	RepoNone      = "none"
	RepoAdmin     = "admin"
	RepoEditor    = "editor"
	RepoViewer    = "viewer"

	// MaxTuples bounds one read.
	MaxTuples      = 5000
	MaxTeams       = 128
	MaxMemberships = 256
)

// RepoObject is the repository object ID of org's entry for graph.
func RepoObject(orgID, graph string) string { return orgID + "|" + graph }

// SplitRepoObject undoes RepoObject.
func SplitRepoObject(id string) (orgID, graph string, ok bool) { return strings.Cut(id, "|") }

// repoRelationRole maps repository relations to the role names routes use.
var repoRelationRole = map[string]Role{RepoAdmin: Admin, RepoEditor: Member, RepoViewer: Viewer, RepoNone: None}

// RepoRelation is the relation granting r on a repository ("" for none).
func RepoRelation(r Role) string {
	for rel, role := range repoRelationRole {
		if role == r {
			return rel
		}
	}
	return ""
}

// RepoAccess is a user's access to one organisation's entry for a graph.
type RepoAccess struct {
	OrgID string `json:"org_id"`
	Graph string `json:"graph"`
	Role  Role   `json:"role"` // Admin, Member (editor) or Viewer
	// Via explains the strongest path: "organisation", "user" or "team".
	Via      string `json:"via"`
	Revision int64  `json:"revision"`
	// Personal: a member's own repository, not the organisation's.
	Personal bool `json:"personal,omitempty"`
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

// AccessOf evaluates a bounded snapshot of current account policy. A partial
// membership or tuple result never authorizes access.
func (s *Service) AccessOf(ctx context.Context, userID string) (Access, error) {
	a := Access{UserID: userID, Orgs: map[string]Role{}, Teams: map[string]string{}}
	err := s.St.ReadSnapshot(ctx, func(st Store) error {
		scoped := *s
		scoped.St = st
		var err error
		a, err = scoped.accessOf(ctx, userID)
		return err
	})
	return a, err
}

func (s *Service) accessOf(ctx context.Context, userID string) (Access, error) {
	a := Access{UserID: userID, Orgs: map[string]Role{}, Teams: map[string]string{}}
	u, err := s.St.UserByID(ctx, userID)
	if err != nil {
		return a, err
	}
	if u.DisabledAt != nil {
		return a, ErrSuspended
	}
	// Before memberships and grants: none of them give root business access.
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return a, err
	}
	ms, err := s.St.UserMemberships(ctx, userID)
	if err != nil {
		return a, err
	}
	if len(ms) > MaxMemberships {
		return a, ErrAccessLimit
	}
	orgs := map[string]Org{}
	for _, m := range ms {
		a.Orgs[m.Org.ID] = m.Role
		orgs[m.Org.ID] = m.Org
	}
	teamTuples, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return a, err
	}
	if len(teamTuples) > MaxTeams {
		return a, ErrAccessLimit
	}

	teamIDs := make([]string, 0, len(teamTuples))
	for _, t := range teamTuples {
		teamIDs = append(teamIDs, t.ObjectID)
	}
	storedTeams, err := s.St.TeamsByIDs(ctx, teamIDs)
	if err != nil {
		return a, err
	}
	teamOrgs := map[string]string{}
	for _, team := range storedTeams {
		teamOrgs[team.ID] = team.OrgID
	}
	teamIDs = teamIDs[:0]
	for _, t := range teamTuples {
		if t.Relation != RelMember && t.Relation != RelMaintainer {
			return a, ErrInvalidInput
		}
		if _, ok := orgs[teamOrgs[t.ObjectID]]; !ok {
			continue
		}
		if _, duplicate := a.Teams[t.ObjectID]; duplicate {
			return a, ErrInvalidInput
		}
		a.Teams[t.ObjectID] = t.Relation
		teamIDs = append(teamIDs, t.ObjectID)
	}
	var teams []Tuple
	if len(teamIDs) > 0 {
		teams, err = s.St.ReadTuples(ctx, TupleFilter{SubjectType: TypeTeam, SubjectIDs: teamIDs, SubjectRelation: RelMember})
		if err != nil {
			return a, err
		}
	}
	direct, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return a, err
	}
	directByRepo := map[string]Tuple{}
	for _, t := range direct {
		if err := validRepoTuple(t); err != nil {
			return a, err
		}
		if _, duplicate := directByRepo[t.ObjectID]; duplicate {
			return a, ErrInvalidInput
		}
		directByRepo[t.ObjectID] = t
	}
	teamBest := map[string]Role{}
	seen := map[string]bool{}
	for _, t := range teams {
		if t.ObjectType == TypeOrganisation {
			if teamOrgs[t.SubjectID] == t.ObjectID && Role(t.Relation).Valid() {
				a.Orgs[t.ObjectID] = Max(a.Orgs[t.ObjectID], Role(t.Relation))
			}
			continue
		}
		if t.ObjectType != TypeRepository {
			return a, ErrInvalidInput
		}
		orgID, _, ok := SplitRepoObject(t.ObjectID)
		if !ok || teamOrgs[t.SubjectID] != orgID {
			continue
		}
		if err := validRepoTuple(t); err != nil {
			return a, err
		}
		key := t.ObjectID + "@" + t.SubjectID
		if seen[key] {
			return a, ErrInvalidInput
		}
		seen[key] = true
		teamBest[t.ObjectID] = Max(teamBest[t.ObjectID], repoRelationRole[t.Relation])
	}
	orgIDs := make([]string, 0, len(orgs))
	for id := range orgs {
		orgIDs = append(orgIDs, id)
	}
	repos, err := s.St.RepositoriesForOrgs(ctx, orgIDs)
	if err != nil {
		return a, err
	}
	for _, repo := range repos {
		orgID := repo.OrgID
		org, ok := orgs[orgID]
		if !ok {
			return a, ErrForbidden
		}
		if !repo.Usable() {
			continue
		}
		key := RepoObject(orgID, repo.Graph)
		role, via := Role(""), TypeOrganisation
		if d, ok := directByRepo[key]; ok {
			role, via = repoRelationRole[d.Relation], TypeUser
		} else if t, ok := teamBest[key]; ok {
			role, via = t, TypeTeam
		} else if !repo.Personal() && org.DefaultRepoAccess == DefaultRepoReadOnly && membershipRole(ms, orgID) != Guest {
			role = Viewer
		}
		if role != Viewer && role != Member && role != Admin {
			continue
		}
		a.Repos = append(a.Repos, RepoAccess{OrgID: orgID, Graph: repo.Graph, Role: role, Via: via, Revision: org.PolicyRevision, Personal: repo.Personal()})
	}
	return a, nil
}

func membershipRole(ms []Membership, orgID string) Role {
	for _, m := range ms {
		if m.Org.ID == orgID {
			return m.Role
		}
	}
	return ""
}

// ResolveRepository enforces the same account policy for API and MCP actions.
func (s *Service) ResolveRepository(ctx context.Context, p Principal, orgID, graph string, least Role) (RepoAccess, error) {
	if least != Viewer && least != Member && least != Admin {
		return RepoAccess{}, ErrForbidden
	}
	if p.OrgID != "" && p.OrgID != orgID {
		return RepoAccess{}, ErrNotFound
	}
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return RepoAccess{}, err
	}
	a, err := s.AccessOf(ctx, p.User.ID)
	if err != nil {
		return RepoAccess{}, err
	}
	for _, r := range a.Repos {
		if r.OrgID == orgID && r.Graph == graph {
			if !r.Role.AtLeast(least) {
				return RepoAccess{}, ErrForbidden
			}
			return r, nil
		}
	}
	return RepoAccess{}, ErrNotFound
}

// ValidatePrincipal rechecks credentials carried by queued work or resumed calls.
func (s *Service) ValidatePrincipal(ctx context.Context, p Principal) error {
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil || u.DisabledAt != nil {
		return ErrUnauthorized
	}
	if p.Via == "key" {
		keys, err := s.St.ListAPIKeys(ctx, p.User.ID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k.ID == p.KeyID && k.OrgID == p.OrgID && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(s.now())) {
				if _, err := s.St.MemberRole(ctx, k.OrgID, u.ID); err != nil {
					return ErrForbidden
				}
				return nil
			}
		}
		return ErrKeyRevoked
	}
	if p.Via == "automation" && p.OrgID != "" {
		return nil
	}
	if p.Via != "session" {
		return ErrUnauthorized
	}
	if p.SessionHash != "" {
		sess, err := s.St.SessionByHash(ctx, p.SessionHash)
		if err != nil || sess.UserID != u.ID || !sess.ExpiresAt.After(s.now()) {
			return ErrUnauthorized
		}
	}
	return nil
}

// validRepoTuple checks a repository grant against the schema.
func validRepoTuple(t Tuple) error {
	if t.ObjectType != TypeRepository || repoRelationRole[t.Relation] == "" || (t.Relation == RepoNone && t.SubjectType != TypeUser) {
		return fmt.Errorf("%w: repository relations are admin, editor, viewer and user-only none", ErrInvalidInput)
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
	// Personal: the entry used is a member's own repository.
	Personal bool
}

// Scope selects only the caller's explicitly enrolled organisations. A key in
// one organisation cannot borrow another organisation's grant or content.
func (a Access) Scope(selected map[string]bool) map[string]Reach {
	out := map[string]Reach{}
	for _, r := range a.Repos {
		if !selected[r.OrgID] || a.Orgs[r.OrgID] == "" {
			continue
		}
		cur, exists := out[r.Graph]
		if exists && cur.OrgID != r.OrgID {
			cur.Ambiguous = true
			cur.Role = Max(cur.Role, r.Role)
			out[r.Graph] = cur
		} else {
			out[r.Graph] = Reach{Role: r.Role, OrgID: r.OrgID, Personal: r.Personal}
		}
	}
	return out
}
