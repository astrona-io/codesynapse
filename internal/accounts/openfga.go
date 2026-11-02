package accounts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// OpenFGA sync (docs/plans/OPENFGA_PLAN.md, phase 5). PostgreSQL stays the
// source of truth: OrgPolicy is what one organisation's tuples in OpenFGA
// must say, and OpenFGASync records how far OpenFGA has caught up. Nothing
// here decides access; AccessOf still does until phase 6.
//
// Custom roles are mirrored by their permissions: a grant becomes the
// role's permissions on the object it names, read from custom_roles in the
// same snapshot. Changing a role raises the organisation's policy revision,
// so the next sync writes the new permissions for every grant of it.

// OpenFGASync is the policy revision of one organisation that OpenFGA
// accepted, in which store and under which model.
type OpenFGASync struct {
	OrgID    string    `json:"org_id"`
	StoreID  string    `json:"store_id"`
	ModelID  string    `json:"model_id"`
	Revision int64     `json:"revision"`
	SyncedAt time.Time `json:"synced_at"`
	// RebuildRequested is set while a rebuild asked for after the last
	// sync has not run yet.
	RebuildRequested *time.Time `json:"rebuild_requested,omitempty"`
}

// OpenFGASyncStore records sync progress. It is separate from Store so the
// sync can be wired without every Store user knowing about it; both stores
// implement it and accountstest.RunOpenFGASync is the contract.
type OpenFGASyncStore interface {
	// OpenFGAPending lists organisations OpenFGA is behind on for this
	// store and model: never synced, synced into another store or model,
	// an older revision than policy_revision, or a rebuild requested. At
	// most limit (bounded by MaxTuples), ordered by ID.
	OpenFGAPending(ctx context.Context, storeID, modelID string, limit int) ([]Org, error)
	// OpenFGASynced returns the organisation's record; ErrNotFound when it
	// was never synced.
	OpenFGASynced(ctx context.Context, orgID string) (OpenFGASync, error)
	// RecordOpenFGASync stores a sync that started at started. A revision
	// never goes backwards within one store and model, so a slow sync that
	// finishes after a newer one cannot lower it. A rebuild request made
	// before started is cleared; a later one stays.
	RecordOpenFGASync(ctx context.Context, s OpenFGASync, started time.Time) error
	// RequestOpenFGARebuild asks for a full rebuild of orgID, or of every
	// organisation when orgID is empty, and returns how many were marked.
	// It never lowers a synced revision, so a rebuild request does not make
	// fresh organisations look stale.
	RequestOpenFGARebuild(ctx context.Context, orgID string, at time.Time) (int, error)
}

// OrgPolicy is the access policy of one organisation in the shape the
// OpenFGA model needs. Only what grants something is kept: members who may
// use the business side (active, not root), usable entries, and grants of
// the organisation's own teams to its members.
type OrgPolicy struct {
	Org     Org
	Members []PolicyMember
	Teams   []PolicyTeam
	Repos   []PolicyRepo
}

// PolicyMember is an eligible member, their membership role and what
// custom organisation roles granted to them directly add.
type PolicyMember struct {
	UserID string
	Role   Role
	Perms  Perms
}

// PolicyTeam is a team of the organisation, its eligible members and the
// organisation role it holds ("" for none) and what custom organisation
// roles granted to it add for its members.
type PolicyTeam struct {
	ID      string
	Role    Role
	Perms   Perms
	Members []PolicyTeamMember
}

// PolicyTeamMember is one eligible member of a team.
type PolicyTeamMember struct {
	UserID     string
	Maintainer bool
}

// PolicyRepo is one usable entry and its grants. Grant roles are Full,
// Write, Read or Deny (an explicit deny, users only), or a custom role.
type PolicyRepo struct {
	Graph    string
	Personal bool
	Users    []PolicyGrant
	Teams    []PolicyGrant
}

// PolicyGrant grants Role to a user or team. A custom role grant has no
// Role, Custom set and the role's repository permissions in Perms: none
// when the role is gone, belongs to another organisation or holds
// organisation permissions. A user's direct custom grant still replaces
// their team grants and the default then, as in AccessOf.
type PolicyGrant struct {
	SubjectID string
	Role      Role
	Custom    bool
	Perms     Perms
}

// touchUserOrgs bumps the policy revision of every organisation userID
// belongs to, for changes to the account itself (suspension, platform
// administration) that change what its memberships grant. The audit event
// of such a change names no organisation, so policyChange would not bump
// one, and OpenFGA would keep the old tuples until a full rebuild.
func (s *Service) touchUserOrgs(ctx context.Context, userID string) error {
	if !s.policy {
		return nil
	}
	ms, err := s.St.UserMemberships(ctx, userID)
	if err != nil {
		return err
	}
	// No MaxMemberships cap here: a suspension must never fail for it.
	for _, m := range ms {
		s.policyOrgs[m.Org.ID] = true
	}
	return nil
}

// OrgPolicy reads orgID's policy from one consistent snapshot. Invalid or
// oversized policy is an error rather than a partial answer, so the sync
// never writes part of it.
func (s *Service) OrgPolicy(ctx context.Context, orgID string) (OrgPolicy, error) {
	var p OrgPolicy
	err := s.St.ReadSnapshot(ctx, func(st Store) error {
		var err error
		p, err = orgPolicy(ctx, st, orgID)
		return err
	})
	return p, err
}

func orgPolicy(ctx context.Context, st Store, orgID string) (OrgPolicy, error) {
	p := OrgPolicy{}
	org, err := st.OrgByID(ctx, orgID)
	if err != nil {
		return p, err
	}
	p.Org = org
	members, err := st.OrgMembers(ctx, orgID)
	if err != nil {
		return p, err
	}
	teams, err := st.OrgTeams(ctx, orgID)
	if err != nil {
		return p, err
	}
	repos, err := st.OrgRepositories(ctx, orgID)
	if err != nil {
		return p, err
	}
	items := len(members) + len(teams) + len(repos)
	if items > MaxTuples {
		return p, ErrAccessLimit
	}
	eligible := map[string]bool{}
	for _, m := range members {
		if m.User.DisabledAt != nil || !m.Role.Valid() {
			continue
		}
		if err := businessAllowed(ctx, st, m.User); err != nil {
			if errors.Is(err, ErrRootAccount) {
				continue
			}
			return p, err
		}
		eligible[m.User.ID] = true
		p.Members = append(p.Members, PolicyMember{UserID: m.User.ID, Role: m.Role})
	}

	teamIDs := make([]string, 0, len(teams))
	byTeam := map[string]*PolicyTeam{}
	p.Teams = make([]PolicyTeam, len(teams))
	for i, t := range teams {
		p.Teams[i] = PolicyTeam{ID: t.ID}
		teamIDs = append(teamIDs, t.ID)
	}
	for i := range p.Teams {
		byTeam[p.Teams[i].ID] = &p.Teams[i]
	}
	stored, err := st.CustomRoles(ctx, []string{orgID})
	if err != nil {
		return p, err
	}
	// custom is what a role relation grants on resource: the role's
	// permissions when it is this organisation's and of that resource,
	// nothing otherwise (like AccessOf).
	custom := func(relation, resource string) (Perms, bool) {
		id, ok := CustomRoleID(relation)
		if !ok {
			return 0, false
		}
		for _, r := range stored {
			if r.ID == id && r.OrgID == orgID && r.Resource == resource {
				return r.Perms(), true
			}
		}
		return 0, true
	}
	// Custom organisation roles of members; those of teams are read with
	// the team roles below.
	orgGrants, err := st.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: TypeUser})
	if err != nil {
		return p, err
	}
	if items += len(orgGrants); items > MaxTuples {
		return p, ErrAccessLimit
	}
	memberPerms := map[string]Perms{}
	for _, t := range orgGrants {
		if perms, ok := custom(t.Relation, ResourceOrganisation); ok && eligible[t.SubjectID] && t.SubjectRelation == "" {
			memberPerms[t.SubjectID] |= perms
		}
	}
	for i := range p.Members {
		p.Members[i].Perms = memberPerms[p.Members[i].UserID]
	}
	if len(teamIDs) > 0 {
		tuples, err := st.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectIDs: teamIDs})
		if err != nil {
			return p, err
		}
		if items += len(tuples); items > MaxTuples {
			return p, ErrAccessLimit
		}
		seen := map[string]bool{}
		for _, t := range tuples {
			team := byTeam[t.ObjectID]
			if team == nil || t.SubjectType != TypeUser || t.SubjectRelation != "" || (t.Relation != TeamRead && t.Relation != TeamFull) {
				return p, fmt.Errorf("%w: team tuple %s#%s", ErrInvalidInput, t.ObjectID, t.Relation)
			}
			// AccessOf refuses a user with two tuples on one team.
			key := t.ObjectID + "@" + t.SubjectID
			if seen[key] {
				return p, fmt.Errorf("%w: duplicate team membership", ErrInvalidInput)
			}
			seen[key] = true
			if eligible[t.SubjectID] {
				team.Members = append(team.Members, PolicyTeamMember{UserID: t.SubjectID, Maintainer: t.Relation == TeamFull})
			}
		}
		roles, err := st.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: TypeTeam})
		if err != nil {
			return p, err
		}
		if items += len(roles); items > MaxTuples {
			return p, ErrAccessLimit
		}
		for _, t := range roles {
			team := byTeam[t.SubjectID]
			// Like AccessOf: only the organisation's own teams' members, and
			// only valid roles, raise an organisation role.
			if team == nil || t.SubjectRelation != TeamRead {
				continue
			}
			if perms, ok := custom(t.Relation, ResourceOrganisation); ok {
				team.Perms |= perms
			} else if Role(t.Relation).Valid() {
				team.Role = Max(team.Role, Role(t.Relation))
			}
		}
	}

	objects := []string{}
	byObject := map[string]*PolicyRepo{}
	for _, r := range repos {
		if r.OrgID != orgID || !r.Usable() {
			continue
		}
		p.Repos = append(p.Repos, PolicyRepo{Graph: r.Graph, Personal: r.Personal()})
		objects = append(objects, RepoObject(orgID, r.Graph))
	}
	for i := range p.Repos {
		byObject[RepoObject(orgID, p.Repos[i].Graph)] = &p.Repos[i]
	}
	if len(objects) > 0 {
		grants, err := st.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectIDs: objects})
		if err != nil {
			return p, err
		}
		if items += len(grants); items > MaxTuples {
			return p, ErrAccessLimit
		}
		seen := map[string]bool{}
		for _, t := range grants {
			repo := byObject[t.ObjectID]
			if repo == nil {
				return p, fmt.Errorf("%w: grant on an unknown entry", ErrInvalidInput)
			}
			if err := validRepoTuple(t); err != nil {
				return p, err
			}
			key := t.ObjectID + "@" + t.SubjectType + ":" + t.SubjectID
			if seen[key] {
				return p, fmt.Errorf("%w: duplicate repository grant", ErrInvalidInput)
			}
			seen[key] = true
			g := PolicyGrant{SubjectID: t.SubjectID, Role: repoRelationRole[t.Relation]}
			if perms, ok := custom(t.Relation, ResourceRepository); ok {
				g = PolicyGrant{SubjectID: t.SubjectID, Custom: true, Perms: perms}
			}
			switch t.SubjectType {
			case TypeUser:
				if eligible[t.SubjectID] {
					repo.Users = append(repo.Users, g)
				}
			case TypeTeam:
				// Grants of another organisation's team grant nothing.
				if byTeam[t.SubjectID] != nil {
					repo.Teams = append(repo.Teams, g)
				}
			}
		}
	}
	sortPolicy(&p)
	return p, nil
}

func sortPolicy(p *OrgPolicy) {
	slices.SortFunc(p.Members, func(a, b PolicyMember) int { return cmp.Compare(a.UserID, b.UserID) })
	slices.SortFunc(p.Teams, func(a, b PolicyTeam) int { return cmp.Compare(a.ID, b.ID) })
	for i := range p.Teams {
		slices.SortFunc(p.Teams[i].Members, func(a, b PolicyTeamMember) int { return cmp.Compare(a.UserID, b.UserID) })
	}
	byGrant := func(a, b PolicyGrant) int { return cmp.Compare(a.SubjectID, b.SubjectID) }
	slices.SortFunc(p.Repos, func(a, b PolicyRepo) int { return cmp.Compare(a.Graph, b.Graph) })
	for i := range p.Repos {
		slices.SortFunc(p.Repos[i].Users, byGrant)
		slices.SortFunc(p.Repos[i].Teams, byGrant)
	}
}

// ErrNoOpenFGASync means the store does not record OpenFGA syncs.
var ErrNoOpenFGASync = errors.New("the accounts store does not record OpenFGA syncs")

// OpenFGARecords returns the store's sync records.
func (s *Service) OpenFGARecords() (OpenFGASyncStore, error) {
	fs, ok := s.St.(OpenFGASyncStore)
	if !ok {
		return nil, ErrNoOpenFGASync
	}
	return fs, nil
}

// RequestOpenFGARebuild asks the API's sync loop for a full rebuild of
// orgID, or of every organisation when orgID is empty. Platform
// administrators only, in a signed-in session; the request is audited
// before it is stored and refused when the audit fails. It only marks the
// organisations: the one API process holding the api-loops lease does the
// writing.
func (s *Service) RequestOpenFGARebuild(ctx context.Context, p Principal, orgID string) (int, error) {
	if err := s.requirePlatform(ctx, p); err != nil {
		return 0, err
	}
	return s.requestOpenFGARebuild(ctx, actorEvent(p, "openfga.rebuild", orgID, orgID), orgID)
}

// OperatorOpenFGARebuild is RequestOpenFGARebuild for the operator's
// command line (codesynapse-api openfga-rebuild).
func (s *Service) OperatorOpenFGARebuild(ctx context.Context, orgID string) (int, error) {
	return s.requestOpenFGARebuild(ctx, Event{Via: "operator", Action: "openfga.rebuild", OrgID: orgID, Target: orgID}, orgID)
}

func (s *Service) requestOpenFGARebuild(ctx context.Context, e Event, orgID string) (int, error) {
	fs, err := s.OpenFGARecords()
	if err != nil {
		return 0, err
	}
	if orgID != "" {
		if _, err := s.St.OrgByID(ctx, orgID); err != nil {
			return 0, err
		}
	}
	if _, err := s.Record(ctx, e); err != nil {
		return 0, fmt.Errorf("audit: %w", err)
	}
	return fs.RequestOpenFGARebuild(ctx, orgID, s.now())
}
