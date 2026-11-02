package accounts

import (
	"context"
	"errors"
	"fmt"
)

// TeamMember is a user's place in a team.
type TeamMember struct {
	User     User   `json:"user"`
	Relation string `json:"relation"` // read or full (also manages the team)
}

// RepoGrant is a grant on a repository, with a readable subject.
type RepoGrant struct {
	Tuple
	Role Role `json:"role"`
	// CustomRole is the custom role the grant names (Role is then "").
	CustomRole *CustomRole `json:"custom_role,omitempty"`
	// Label names the subject: a user's email or "org/team".
	Label string `json:"label"`
	// External is true for subjects outside the owning organisation.
	External bool `json:"external"`
}

// CreateTeam adds a team to an organisation (owners and admins).
func (s *Service) CreateTeam(ctx context.Context, p Principal, orgID, name string) (Team, error) {
	return policyChange(ctx, s, func(tx *Service) (Team, error) { return tx.createTeamPolicy(ctx, p, orgID, name) })
}

func (s *Service) createTeamPolicy(ctx context.Context, p Principal, orgID, name string) (Team, error) {
	if p.Via == "key" {
		return Team{}, ErrForbidden
	}
	if err := s.Require(ctx, p, orgID, OrgTeamsFull); err != nil {
		return Team{}, err
	}
	name, err := checkName(name, "a team name")
	if err != nil {
		return Team{}, err
	}
	slug := Slugify(name)
	if !slugRe.MatchString(slug) {
		return Team{}, fmt.Errorf("%w: the team name needs letters or digits", ErrInvalidInput)
	}
	id, err := newID()
	if err != nil {
		return Team{}, err
	}
	t := Team{ID: id, OrgID: orgID, Slug: slug, Name: name, CreatedAt: s.now()}
	if err := s.St.CreateTeam(ctx, t); err != nil {
		return Team{}, err
	}
	s.audit(ctx, actorEvent(p, "team.create", orgID, slug))
	return t, nil
}

// DeleteTeam removes a team and every grant it held.
func (s *Service) DeleteTeam(ctx context.Context, p Principal, teamID string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) { return struct{}{}, tx.deleteTeamPolicy(ctx, p, teamID) })
	return err
}

func (s *Service) deleteTeamPolicy(ctx context.Context, p Principal, teamID string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	t, err := s.St.TeamByID(ctx, teamID)
	if err != nil {
		return err
	}
	if err := s.Require(ctx, p, t.OrgID, OrgTeamsFull); err != nil {
		return err
	}
	admins, err := s.adminRepositories(ctx, t.OrgID)
	if err != nil {
		return err
	}
	if err := s.St.DeleteTeam(ctx, teamID); err != nil {
		return err
	}
	if err := s.preserveAdmins(ctx, t.OrgID, admins); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "team.delete", t.OrgID, t.Slug))
	return nil
}

// canMaintain: organisation owners and admins, and the team's maintainers.
func (s *Service) canMaintain(ctx context.Context, p Principal, t Team) error {
	if err := s.Require(ctx, p, t.OrgID, OrgTeamsFull); err == nil {
		return nil
	}
	if _, err := s.RoleIn(ctx, p, t.OrgID); err != nil {
		return err
	}
	ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: t.ID, Relation: TeamFull, SubjectType: TypeUser, SubjectID: p.User.ID})
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		return ErrForbidden
	}
	return nil
}

// SetTeamMember adds a member of the team's organisation (guests included)
// as read or full, replacing their previous place.
func (s *Service) SetTeamMember(ctx context.Context, p Principal, teamID, email, relation string) (TeamMember, error) {
	return policyChange(ctx, s, func(tx *Service) (TeamMember, error) { return tx.setTeamMemberPolicy(ctx, p, teamID, email, relation) })
}

func (s *Service) setTeamMemberPolicy(ctx context.Context, p Principal, teamID, email, relation string) (TeamMember, error) {
	if p.Via == "key" {
		return TeamMember{}, ErrForbidden
	}
	if relation != TeamRead && relation != TeamFull {
		return TeamMember{}, fmt.Errorf("%w: a team relation is read or full", ErrInvalidInput)
	}
	t, err := s.St.TeamByID(ctx, teamID)
	if err != nil {
		return TeamMember{}, err
	}
	if err := s.canMaintain(ctx, p, t); err != nil {
		return TeamMember{}, err
	}
	u, err := s.MemberByEmail(ctx, t.OrgID, email)
	if err != nil {
		return TeamMember{}, err
	}
	places, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, SubjectType: TypeUser, SubjectID: u.ID})
	if err != nil {
		return TeamMember{}, err
	}
	existing := false
	for _, place := range places {
		if place.ObjectID == t.ID {
			existing = true
		}
	}
	if !existing && len(places) >= MaxTeams {
		return TeamMember{}, ErrAccessLimit
	}
	if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: t.ID, SubjectType: TypeUser, SubjectID: u.ID}); err != nil {
		return TeamMember{}, err
	}
	if err := s.St.WriteTuple(ctx, Tuple{ObjectType: TypeTeam, ObjectID: t.ID, Relation: relation, SubjectType: TypeUser, SubjectID: u.ID,
		CreatedBy: p.User.ID, CreatedAt: s.now()}); err != nil {
		return TeamMember{}, err
	}
	e := actorEvent(p, "team.member.set", t.OrgID, t.Slug+" "+u.Email)
	e.Detail = relation
	s.audit(ctx, e)
	return TeamMember{User: u, Relation: relation}, nil
}

// RemoveTeamMember takes a user out of a team.
func (s *Service) RemoveTeamMember(ctx context.Context, p Principal, teamID, userID string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		return struct{}{}, tx.removeTeamMemberPolicy(ctx, p, teamID, userID)
	})
	return err
}

func (s *Service) removeTeamMemberPolicy(ctx context.Context, p Principal, teamID, userID string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	t, err := s.St.TeamByID(ctx, teamID)
	if err != nil {
		return err
	}
	if userID != p.User.ID {
		if err := s.canMaintain(ctx, p, t); err != nil {
			return err
		}
	}
	admins, err := s.adminRepositories(ctx, t.OrgID)
	if err != nil {
		return err
	}
	if _, err := s.RoleIn(ctx, p, t.OrgID); err != nil {
		return err
	}
	n, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: t.ID, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("team member: %w", ErrNotFound)
	}
	if err := s.preserveAdmins(ctx, t.OrgID, admins); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "team.member.remove", t.OrgID, t.Slug+" "+userID))
	return nil
}

// TeamMembers lists a team (organisation viewers and up, and its members).
func (s *Service) TeamMembers(ctx context.Context, p Principal, teamID string) ([]TeamMember, error) {
	t, err := s.St.TeamByID(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if err := s.Require(ctx, p, t.OrgID, OrgTeamsRead); err != nil {
		if s.canMaintain(ctx, p, t) != nil {
			return nil, err
		}
	}
	ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: t.ID, SubjectType: TypeUser})
	if err != nil {
		return nil, err
	}
	out := []TeamMember{}
	for _, x := range ts {
		u, err := s.St.UserByID(ctx, x.SubjectID)
		if err != nil {
			continue // a deleted user leaves no member
		}
		u.PasswordHash = ""
		out = append(out, TeamMember{User: u, Relation: x.Relation})
	}
	return out, nil
}

// SetTeamRole gives every member of a team a role in the team's
// organisation ("" removes it). Only owners grant owner.
func (s *Service) SetTeamRole(ctx context.Context, p Principal, teamID string, role Role) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) { return struct{}{}, tx.setTeamRolePolicy(ctx, p, teamID, role) })
	return err
}

func (s *Service) setTeamRolePolicy(ctx context.Context, p Principal, teamID string, role Role) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	t, err := s.St.TeamByID(ctx, teamID)
	if err != nil {
		return err
	}
	mine, err := s.RoleIn(ctx, p, t.OrgID)
	if err != nil {
		return err
	}
	if !mine.Has(OrgTeamsFull) || (role == Owner && mine != Owner) {
		return ErrForbidden
	}
	if role != "" && !role.Valid() {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}
	// Only the built-in role: a custom organisation role stays.
	for _, r := range []Role{Guest, Read, Write, Full, Owner} {
		if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: t.OrgID, Relation: string(r), SubjectType: TypeTeam, SubjectID: t.ID}); err != nil {
			return err
		}
	}
	if role != "" {
		if err := s.St.WriteTuple(ctx, Tuple{ObjectType: TypeOrganisation, ObjectID: t.OrgID, Relation: string(role), SubjectType: TypeTeam,
			SubjectID: t.ID, SubjectRelation: TeamRead, CreatedBy: p.User.ID, CreatedAt: s.now()}); err != nil {
			return err
		}
	}
	e := actorEvent(p, "team.role.set", t.OrgID, t.Slug)
	e.Detail = "role " + string(role)
	s.audit(ctx, e)
	return nil
}

// TeamRole is the organisation role granted to a team ("" without one).
func (s *Service) TeamRole(ctx context.Context, t Team) (Role, error) {
	ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: t.OrgID, SubjectType: TypeTeam, SubjectID: t.ID})
	var best Role
	for _, x := range ts {
		if Role(x.Relation).Valid() {
			best = Max(best, Role(x.Relation))
		}
	}
	return best, err
}

// canAdminRepo requires repository.access:full on the entry and returns
// what p holds there.
func (s *Service) canAdminRepo(ctx context.Context, p Principal, orgID, graph string) (Perms, error) {
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return 0, err
	}

	if p.OrgID != "" && p.OrgID != orgID {
		return 0, ErrForbidden
	}
	a, err := s.AccessFor(ctx, p)
	if err != nil {
		return 0, err
	}
	for _, r := range a.Repos {
		if r.OrgID == orgID && r.Graph == graph && r.Perms.Has(RepoAccessFull) {
			return r.Perms, nil
		}
	}
	return 0, ErrForbidden
}

// SetRepoGrant gives a user or a team's members role on orgID's entry for
// graph, replacing their previous grant there. The subject must be enrolled
// in the owning organisation.
func (s *Service) SetRepoGrant(ctx context.Context, p Principal, orgID, graph string, subject Tuple, role Role) (RepoGrant, error) {
	return policyChange(ctx, s, func(tx *Service) (RepoGrant, error) {
		return tx.setRepoGrantPolicy(ctx, p, orgID, graph, subject, role)
	})
}

// CanGrant reports whether p may change graph's grants. Callers that look a
// subject up by email check it first, so only repository admins, who see the
// grants anyway, learn whether an email belongs to a member.
func (s *Service) CanGrant(ctx context.Context, p Principal, orgID, graph string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	_, err := s.canAdminRepo(ctx, p, orgID, graph)
	return err
}

func (s *Service) setRepoGrantPolicy(ctx context.Context, p Principal, orgID, graph string, subject Tuple, role Role) (RepoGrant, error) {
	if p.Via == "key" {
		return RepoGrant{}, ErrForbidden // agents do not change access
	}
	mine, err := s.canAdminRepo(ctx, p, orgID, graph)
	if err != nil {
		return RepoGrant{}, err
	}
	// role is a built-in role or "role:<id>" for a custom one, which must
	// be this organisation's and hold repository permissions.
	rel, grant := RepoRelation(role), role.Bundle(ResourceRepository)
	if id, ok := CustomRoleID(string(role)); ok {
		r, err := s.customRole(ctx, orgID, id)
		if err != nil {
			return RepoGrant{}, err
		}
		if r.Resource != ResourceRepository {
			return RepoGrant{}, fmt.Errorf("%w: grant organisation roles on the organisation", ErrInvalidInput)
		}
		rel, grant = CustomRelation(r.ID), r.Perms()
	}
	// No escalation: nobody grants a permission they do not hold here.
	if !mine.Covers(grant) {
		return RepoGrant{}, fmt.Errorf("%w: you can only grant permissions you hold", ErrForbidden)
	}
	t := Tuple{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, graph), Relation: rel,
		SubjectType: subject.SubjectType, SubjectID: subject.SubjectID, SubjectRelation: subject.SubjectRelation, CreatedBy: p.User.ID, CreatedAt: s.now()}
	if err := validRepoTuple(t); err != nil {
		return RepoGrant{}, err
	}
	if !s.orgHasRepo(ctx, orgID, graph) {
		return RepoGrant{}, fmt.Errorf("repository: %w", ErrNotFound)
	}
	g, err := s.describe(ctx, orgID, t)
	if err != nil {
		return RepoGrant{}, err
	}
	if g.External {
		return RepoGrant{}, fmt.Errorf("%w: enrol the subject in this organisation before granting access", ErrForbidden)
	}
	if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: t.ObjectType, ObjectID: t.ObjectID, SubjectType: t.SubjectType, SubjectID: t.SubjectID, SubjectRelation: t.SubjectRelation}); err != nil {
		return RepoGrant{}, err
	}
	if err := s.St.WriteTuple(ctx, t); err != nil {
		return RepoGrant{}, err
	}
	if err := s.guardRepoAdmin(ctx, orgID, graph); err != nil {
		return RepoGrant{}, err
	}
	e := actorEvent(p, "repo.grant.set", orgID, graph+" "+g.Label)
	e.Detail = t.Relation
	s.audit(ctx, e)
	return g, nil
}

// RemoveRepoGrant removes a subject's grant on orgID's entry for graph.
func (s *Service) RemoveRepoGrant(ctx context.Context, p Principal, orgID, graph string, subject Tuple) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		return struct{}{}, tx.removeRepoGrantPolicy(ctx, p, orgID, graph, subject)
	})
	return err
}

func (s *Service) removeRepoGrantPolicy(ctx context.Context, p Principal, orgID, graph string, subject Tuple) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	if _, err := s.canAdminRepo(ctx, p, orgID, graph); err != nil {
		return err
	}
	n, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, graph),
		SubjectType: subject.SubjectType, SubjectID: subject.SubjectID, SubjectRelation: subject.SubjectRelation})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("grant: %w", ErrNotFound)
	}
	if err := s.guardRepoAdmin(ctx, orgID, graph); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "repo.grant.remove", orgID, graph+" "+subject.SubjectType+":"+subject.SubjectID))
	return nil
}

// RepoGrants lists the explicit grants on orgID's entry for graph
// (repository administrators only).
func (s *Service) RepoGrants(ctx context.Context, p Principal, orgID, graph string) ([]RepoGrant, error) {
	if _, err := s.canAdminRepo(ctx, p, orgID, graph); err != nil {
		return nil, err
	}
	ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, graph)})
	if err != nil {
		return nil, err
	}
	out := []RepoGrant{}
	for _, t := range ts {
		g, err := s.describe(ctx, orgID, t)
		if err != nil {
			continue // a subject that no longer exists
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Service) orgHasRepo(ctx context.Context, orgID, graph string) bool {
	_, ok := s.orgEntry(ctx, orgID, graph)
	return ok
}

// orgEntry is the organisation's entry for graph, in either space.
func (s *Service) orgEntry(ctx context.Context, orgID, graph string) (Repository, bool) {
	repos, err := s.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return Repository{}, false
	}
	for _, r := range repos {
		if r.Graph == graph {
			return r, true
		}
	}
	return Repository{}, false
}

// describe names a grant's subject and whether it is outside orgID.
func (s *Service) describe(ctx context.Context, orgID string, t Tuple) (RepoGrant, error) {
	g := RepoGrant{Tuple: t, Role: repoRelationRole[t.Relation]}
	if id, ok := CustomRoleID(t.Relation); ok {
		if r, err := s.customRole(ctx, orgID, id); err == nil {
			g.CustomRole = &r
		}
	}
	switch t.SubjectType {
	case TypeUser:
		u, err := s.St.UserByID(ctx, t.SubjectID)
		if err != nil {
			return g, err
		}
		g.Label = u.Email
		_, err = s.St.MemberRole(ctx, orgID, u.ID)
		g.External = errors.Is(err, ErrNotFound)
		if err != nil && !g.External {
			return g, err
		}
	case TypeTeam:
		team, err := s.St.TeamByID(ctx, t.SubjectID)
		if err != nil {
			return g, err
		}
		o, err := s.St.OrgByID(ctx, team.OrgID)
		if err != nil {
			return g, err
		}
		g.Label = o.Slug + "/" + team.Slug
		g.External = team.OrgID != orgID
	default:
		return g, fmt.Errorf("%w: unknown subject", ErrInvalidInput)
	}
	return g, nil
}

// dropUserTuples removes what a user held inside an organisation they
// left: team places and grants on its repositories.
func (s *Service) dropUserTuples(ctx context.Context, orgID, userID string) error {
	teams, err := s.St.OrgTeams(ctx, orgID)
	if err != nil {
		return err
	}
	for _, t := range teams {
		if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: t.ID, SubjectType: TypeUser, SubjectID: userID}); err != nil {
			return err
		}
	}
	grants, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return err
	}
	for _, g := range grants {
		if o, _, ok := SplitRepoObject(g.ObjectID); ok && o == orgID {
			if err := s.St.DeleteTuple(ctx, g); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// DropRepoGrants removes every grant on orgID's entry for graph (the
// organisation unlinked the repository).
func (s *Service) DropRepoGrants(ctx context.Context, orgID, graph string) error {
	_, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, graph)})
	return err
}
