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

// PolicyMember is an eligible member and their membership role.
type PolicyMember struct {
	UserID string
	Role   Role
}

// PolicyTeam is a team of the organisation, its eligible members and the
// organisation role it holds ("" for none).
type PolicyTeam struct {
	ID      string
	Role    Role
	Members []PolicyTeamMember
}

// PolicyTeamMember is one eligible member of a team.
type PolicyTeamMember struct {
	UserID     string
	Maintainer bool
}

// PolicyRepo is one usable entry and its grants. Grant roles are Full,
// Write, Read or Deny (an explicit deny, users only).
type PolicyRepo struct {
	Graph    string
	Personal bool
	Users    []PolicyGrant
	Teams    []PolicyGrant
}

// PolicyGrant grants Role to a user or team.
type PolicyGrant struct {
	SubjectID string
	Role      Role
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
	// Custom organisation roles (to members or teams) are not in the model
	// yet either.
	orgGrants, err := st.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID})
	if err != nil {
		return p, err
	}
	if items += len(orgGrants); items > MaxTuples {
		return p, ErrAccessLimit
	}
	for _, t := range orgGrants {
		if _, custom := CustomRoleID(t.Relation); custom {
			return p, ErrCustomRolesNotMirrored
		}
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
			if team == nil || t.SubjectRelation != TeamRead || !Role(t.Relation).Valid() {
				continue
			}
			team.Role = Max(team.Role, Role(t.Relation))
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
			if _, custom := CustomRoleID(t.Relation); custom {
				return p, ErrCustomRolesNotMirrored
			}
			key := t.ObjectID + "@" + t.SubjectType + ":" + t.SubjectID
			if seen[key] {
				return p, fmt.Errorf("%w: duplicate repository grant", ErrInvalidInput)
			}
			seen[key] = true
			g := PolicyGrant{SubjectID: t.SubjectID, Role: repoRelationRole[t.Relation]}
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

// ErrCustomRolesNotMirrored refuses to mirror an organisation that grants
// custom roles: the OpenFGA model has no relation for them yet, and leaving
// them out would make OpenFGA disagree with AccessOf. The organisation
// stays behind (never fresh) until the model holds custom roles.
var ErrCustomRolesNotMirrored = fmt.Errorf("%w: custom role grants are not in the OpenFGA model yet", ErrInvalidInput)

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
