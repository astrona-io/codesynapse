package accounts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/redact"
)

// Custom roles are named bundles of permissions an organisation defines
// (docs/plans/OPENFGA_PLAN.md, "Custom roles"). A role holds repository or
// organisation permissions, never both, and is granted with a relation
// tuple whose relation is "role:<id>": on a repository to a user or a
// team's members, replacing their built-in grant there, or on the
// organisation to a member or a team, adding to the member's built-in role.
const (
	MaxCustomRoles        = 50
	MaxCustomRoleName     = 64
	MaxCustomRoleDescribe = 500

	customRelationPrefix = "role:"
)

// ErrRoleInUse refuses to delete a role that is still granted.
var ErrRoleInUse = fmt.Errorf("role is still granted; remove its grants first: %w", ErrExists)

// CustomRole is one organisation's named permission bundle.
type CustomRole struct {
	ID          string       `json:"id"`
	OrgID       string       `json:"org_id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Resource    string       `json:"resource"` // repository or organisation
	Permissions []Permission `json:"permissions"`
	CreatedBy   string       `json:"created_by"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// Perms is what the role holds. Permissions of the other resource, which
// a stored role never has, are dropped rather than trusted.
func (r CustomRole) Perms() Perms { return PermsOf(r.Permissions...).Only(r.Resource) }

// CustomRoleInput is what a person writes to create or change a role.
type CustomRoleInput struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Resource    string       `json:"resource"`
	Permissions []Permission `json:"permissions"`
}

// CustomRelation is the tuple relation granting the role with id.
func CustomRelation(id string) string { return customRelationPrefix + id }

// CustomRoleID reads the role ID from a relation ("", false for a
// built-in relation).
func CustomRoleID(relation string) (string, bool) {
	id, ok := strings.CutPrefix(relation, customRelationPrefix)
	return id, ok && id != ""
}

// reservedRoleNames are the built-in and legacy names a custom role may
// not take, so a name never reads as something it is not.
var reservedRoleNames = []string{"guest", "read", "write", "full", "owner", "deny", "none", "viewer", "editor", "member", "maintainer", "admin"}

// checkCustomRole validates and normalises input; free text goes through
// redact.Free before anything else sees it.
func checkCustomRole(in CustomRoleInput) (CustomRole, error) {
	name := strings.Join(strings.Fields(redact.Free(in.Name)), " ")
	if n := utf8.RuneCountInString(name); n < 1 || n > MaxCustomRoleName {
		return CustomRole{}, fmt.Errorf("%w: a role name needs 1 to %d characters", ErrInvalidInput, MaxCustomRoleName)
	}
	if strings.ContainsFunc(name, unicode.IsControl) || strings.ContainsAny(name, ":#|") {
		return CustomRole{}, fmt.Errorf("%w: a role name cannot hold control characters, ':', '#' or '|'", ErrInvalidInput)
	}
	if slices.Contains(reservedRoleNames, strings.ToLower(name)) {
		return CustomRole{}, fmt.Errorf("%w: %q is a built-in role name", ErrInvalidInput, name)
	}
	desc := strings.TrimSpace(redact.Free(in.Description))
	if strings.ContainsFunc(desc, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		return CustomRole{}, fmt.Errorf("%w: a role description cannot hold control characters", ErrInvalidInput)
	}
	if utf8.RuneCountInString(desc) > MaxCustomRoleDescribe {
		return CustomRole{}, fmt.Errorf("%w: a role description has at most %d characters", ErrInvalidInput, MaxCustomRoleDescribe)
	}
	if in.Resource != ResourceRepository && in.Resource != ResourceOrganisation {
		return CustomRole{}, fmt.Errorf("%w: a role holds repository or organisation permissions", ErrInvalidInput)
	}
	if len(in.Permissions) == 0 || len(in.Permissions) > len(permAt) {
		return CustomRole{}, fmt.Errorf("%w: pick at least one permission", ErrInvalidInput)
	}
	var set Perms
	for _, p := range in.Permissions {
		switch {
		case !p.Valid():
			return CustomRole{}, fmt.Errorf("%w: unknown permission %q", ErrInvalidInput, p)
		case p.Marker():
			return CustomRole{}, fmt.Errorf("%w: %s is not a permission a role can hold", ErrInvalidInput, p)
		case p.Resource() != in.Resource:
			return CustomRole{}, fmt.Errorf("%w: %s is not a %s permission", ErrInvalidInput, p, in.Resource)
		}
		set |= PermsOf(p)
	}
	if in.Resource == ResourceRepository {
		// Every other repository permission is about code one can read.
		set |= PermsOf(RepoCodeRead)
	}
	return CustomRole{Name: name, Description: desc, Resource: in.Resource, Permissions: set.List()}, nil
}

// CustomRoles lists orgID's roles for anyone who may read them.
func (s *Service) CustomRoles(ctx context.Context, p Principal, orgID string) ([]CustomRole, error) {
	if err := s.Require(ctx, p, orgID, OrgRolesRead); err != nil {
		return nil, err
	}
	return s.St.CustomRoles(ctx, []string{orgID})
}

// customRole is orgID's role id (ErrNotFound for another organisation's).
func (s *Service) customRole(ctx context.Context, orgID, id string) (CustomRole, error) {
	roles, err := s.St.CustomRoles(ctx, []string{orgID})
	if err != nil {
		return CustomRole{}, err
	}
	for _, r := range roles {
		if r.ID == id && r.OrgID == orgID {
			return r, nil
		}
	}
	return CustomRole{}, fmt.Errorf("role: %w", ErrNotFound)
}

// canManageRoles is the gate for role changes: a signed-in session (never
// an API key) holding organisation.roles:full. It returns the caller's
// organisation permissions and their permissions per repository there.
func (s *Service) canManageRoles(ctx context.Context, p Principal, orgID string) (Perms, map[string]Perms, error) {
	if p.Via != "session" {
		return 0, nil, ErrForbidden
	}
	mine, err := s.PermsIn(ctx, p, orgID)
	if err != nil {
		return 0, nil, err
	}
	if !mine.Has(OrgRolesFull) {
		return 0, nil, ErrForbidden
	}
	a, err := s.AccessOf(ctx, p.User.ID)
	if err != nil {
		return 0, nil, err
	}
	repos := map[string]Perms{}
	for _, r := range a.Repos {
		if r.OrgID == orgID {
			repos[RepoObject(r.OrgID, r.Graph)] = r.Perms
		}
	}
	return mine, repos, nil
}

// noEscalation refuses a role holding a permission its author does not:
// organisation permissions in the organisation, repository permissions on
// at least one repository there and on every repository where the role is
// granted already (an edit reaches every holder at once).
func (s *Service) noEscalation(ctx context.Context, r CustomRole, org Perms, repos map[string]Perms) error {
	want := r.Perms()
	if r.Resource == ResourceOrganisation {
		if !org.Covers(want) {
			return fmt.Errorf("%w: a role can only hold permissions you hold", ErrForbidden)
		}
		return nil
	}
	var held Perms
	for _, p := range repos {
		held |= p
	}
	if !held.Covers(want) {
		return fmt.Errorf("%w: a role can only hold permissions you hold", ErrForbidden)
	}
	if r.ID == "" {
		return nil
	}
	grants, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, Relation: CustomRelation(r.ID)})
	if err != nil {
		return err
	}
	for _, g := range grants {
		if !repos[g.ObjectID].Covers(want) {
			return fmt.Errorf("%w: the role is granted on a repository where you do not hold all of its permissions", ErrForbidden)
		}
	}
	return nil
}

// CreateCustomRole adds a role to orgID.
func (s *Service) CreateCustomRole(ctx context.Context, p Principal, orgID string, in CustomRoleInput) (CustomRole, error) {
	return policyChange(ctx, s, func(tx *Service) (CustomRole, error) {
		org, repos, err := tx.canManageRoles(ctx, p, orgID)
		if err != nil {
			return CustomRole{}, err
		}
		r, err := checkCustomRole(in)
		if err != nil {
			return CustomRole{}, err
		}
		if err := tx.noEscalation(ctx, r, org, repos); err != nil {
			return CustomRole{}, err
		}
		existing, err := tx.St.CustomRoles(ctx, []string{orgID})
		if err != nil {
			return CustomRole{}, err
		}
		if len(existing) >= MaxCustomRoles {
			return CustomRole{}, fmt.Errorf("%w: an organisation has at most %d roles", ErrInvalidInput, MaxCustomRoles)
		}
		if r.ID, err = newID(); err != nil {
			return CustomRole{}, err
		}
		r.OrgID, r.CreatedBy, r.CreatedAt = orgID, p.User.ID, tx.now()
		r.UpdatedAt = r.CreatedAt
		if err := tx.auditFirst(ctx, roleEvent(p, "org.role.create", r)); err != nil {
			return CustomRole{}, err
		}
		return r, tx.St.CreateCustomRole(ctx, r)
	})
}

// UpdateCustomRole changes a role's name, description and permissions; its
// resource stays. Everyone it is granted to gets the new bundle at once.
func (s *Service) UpdateCustomRole(ctx context.Context, p Principal, orgID, id string, in CustomRoleInput) (CustomRole, error) {
	return policyChange(ctx, s, func(tx *Service) (CustomRole, error) {
		org, repos, err := tx.canManageRoles(ctx, p, orgID)
		if err != nil {
			return CustomRole{}, err
		}
		cur, err := tx.customRole(ctx, orgID, id)
		if err != nil {
			return CustomRole{}, err
		}
		if in.Resource == "" {
			in.Resource = cur.Resource
		}
		if in.Resource != cur.Resource {
			return CustomRole{}, fmt.Errorf("%w: a role's resource cannot change", ErrInvalidInput)
		}
		r, err := checkCustomRole(in)
		if err != nil {
			return CustomRole{}, err
		}
		r.ID, r.OrgID, r.CreatedBy, r.CreatedAt, r.UpdatedAt = cur.ID, cur.OrgID, cur.CreatedBy, cur.CreatedAt, tx.now()
		if err := tx.noEscalation(ctx, r, org, repos); err != nil {
			return CustomRole{}, err
		}
		if err := tx.auditFirst(ctx, roleEvent(p, "org.role.update", r)); err != nil {
			return CustomRole{}, err
		}
		return r, tx.St.UpdateCustomRole(ctx, r)
	})
}

// DeleteCustomRole removes a role nobody is granted any more.
func (s *Service) DeleteCustomRole(ctx context.Context, p Principal, orgID, id string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if _, _, err := tx.canManageRoles(ctx, p, orgID); err != nil {
			return struct{}{}, err
		}
		r, err := tx.customRole(ctx, orgID, id)
		if err != nil {
			return struct{}{}, err
		}
		grants, err := tx.St.ReadTuples(ctx, TupleFilter{Relation: CustomRelation(id)})
		if err != nil {
			return struct{}{}, err
		}
		if len(grants) > 0 {
			return struct{}{}, ErrRoleInUse
		}
		if err := tx.auditFirst(ctx, roleEvent(p, "org.role.delete", r)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, tx.St.DeleteCustomRole(ctx, orgID, id)
	})
	return err
}

func roleEvent(p Principal, action string, r CustomRole) Event {
	e := actorEvent(p, action, r.OrgID, r.ID)
	perms := make([]string, len(r.Permissions))
	for i, x := range r.Permissions {
		perms[i] = string(x)
	}
	e.Detail = r.Resource + " " + r.Name + ": " + strings.Join(perms, " ")
	return e
}

// auditFirst records e before the change it describes; a failed write
// refuses the change. Inside a policy change the entry commits with it.
func (s *Service) auditFirst(ctx context.Context, e Event) error {
	if s.policy && e.OrgID != "" {
		s.policyOrgs[e.OrgID] = true
	}
	if _, err := s.Record(ctx, e); err != nil {
		return errors.Join(ErrAuditUnavailable, err)
	}
	return nil
}

// OrgRoleGrant is a custom organisation role held by a member or a team.
type OrgRoleGrant struct {
	Tuple
	Role  CustomRole `json:"role"`
	Label string     `json:"label"`
}

// SetOrgRoleGrant gives a member (subject user) or a team's members
// (subject team#read) the custom organisation role id in orgID, replacing
// the custom organisation role they held. Their built-in role stays.
func (s *Service) SetOrgRoleGrant(ctx context.Context, p Principal, orgID string, subject Tuple, id string) (OrgRoleGrant, error) {
	return policyChange(ctx, s, func(tx *Service) (OrgRoleGrant, error) {
		if p.Via != "session" {
			return OrgRoleGrant{}, ErrForbidden
		}
		mine, err := tx.PermsIn(ctx, p, orgID)
		if err != nil {
			return OrgRoleGrant{}, err
		}
		g, err := tx.orgRoleSubject(ctx, orgID, subject, mine)
		if err != nil {
			return OrgRoleGrant{}, err
		}
		r, err := tx.customRole(ctx, orgID, id)
		if err != nil {
			return OrgRoleGrant{}, err
		}
		if r.Resource != ResourceOrganisation {
			return OrgRoleGrant{}, fmt.Errorf("%w: grant repository roles on a repository", ErrInvalidInput)
		}
		if !mine.Covers(r.Perms()) {
			return OrgRoleGrant{}, fmt.Errorf("%w: you can only grant permissions you hold", ErrForbidden)
		}
		g.Role = r
		g.Relation = CustomRelation(r.ID)
		g.CreatedBy, g.CreatedAt = p.User.ID, tx.now()
		e := actorEvent(p, "org.role.grant", orgID, g.Label)
		e.Detail = r.Name
		if err := tx.auditFirst(ctx, e); err != nil {
			return OrgRoleGrant{}, err
		}
		if err := tx.dropOrgRoleGrants(ctx, orgID, subject); err != nil {
			return OrgRoleGrant{}, err
		}
		return g, tx.St.WriteTuple(ctx, g.Tuple)
	})
}

// RemoveOrgRoleGrant takes the custom organisation role from a subject.
func (s *Service) RemoveOrgRoleGrant(ctx context.Context, p Principal, orgID string, subject Tuple) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via != "session" {
			return struct{}{}, ErrForbidden
		}
		mine, err := tx.PermsIn(ctx, p, orgID)
		if err != nil {
			return struct{}{}, err
		}
		g, err := tx.orgRoleSubject(ctx, orgID, subject, mine)
		if err != nil {
			return struct{}{}, err
		}
		held, err := tx.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: g.SubjectType, SubjectID: g.SubjectID, SubjectRelation: g.SubjectRelation})
		if err != nil {
			return struct{}{}, err
		}
		if !slices.ContainsFunc(held, func(t Tuple) bool { _, ok := CustomRoleID(t.Relation); return ok }) {
			return struct{}{}, fmt.Errorf("grant: %w", ErrNotFound)
		}
		if err := tx.auditFirst(ctx, actorEvent(p, "org.role.revoke", orgID, g.Label)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, tx.dropOrgRoleGrants(ctx, orgID, subject)
	})
	return err
}

// orgRoleSubject checks who the caller may give an organisation role:
// members need organisation.members:full, teams organisation.teams:full,
// and the subject must belong to orgID.
func (s *Service) orgRoleSubject(ctx context.Context, orgID string, subject Tuple, mine Perms) (OrgRoleGrant, error) {
	g := OrgRoleGrant{Tuple: Tuple{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: subject.SubjectType, SubjectID: subject.SubjectID}}
	switch subject.SubjectType {
	case TypeUser:
		if !mine.Has(OrgMembersFull) {
			return g, ErrForbidden
		}
		u, err := s.St.UserByID(ctx, subject.SubjectID)
		if err != nil {
			return g, err
		}
		if _, err := s.St.MemberRole(ctx, orgID, u.ID); err != nil {
			return g, err
		}
		g.Label = u.Email
	case TypeTeam:
		if !mine.Has(OrgTeamsFull) {
			return g, ErrForbidden
		}
		t, err := s.St.TeamByID(ctx, subject.SubjectID)
		if err != nil {
			return g, err
		}
		if t.OrgID != orgID {
			return g, fmt.Errorf("team: %w", ErrNotFound)
		}
		g.SubjectRelation, g.Label = TeamRead, t.Slug
	default:
		return g, fmt.Errorf("%w: grant to a member or a team", ErrInvalidInput)
	}
	return g, nil
}

func (s *Service) dropOrgRoleGrants(ctx context.Context, orgID string, subject Tuple) error {
	rel := subject.SubjectRelation
	if subject.SubjectType == TypeTeam {
		rel = TeamRead
	}
	held, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: subject.SubjectType, SubjectID: subject.SubjectID, SubjectRelation: rel})
	if err != nil {
		return err
	}
	for _, t := range held {
		if _, ok := CustomRoleID(t.Relation); ok {
			if err := s.St.DeleteTuple(ctx, t); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// OrgRoleGrants lists the custom organisation roles held in orgID.
func (s *Service) OrgRoleGrants(ctx context.Context, p Principal, orgID string) ([]OrgRoleGrant, error) {
	if err := s.Require(ctx, p, orgID, OrgRolesRead); err != nil {
		return nil, err
	}
	roles, err := s.St.CustomRoles(ctx, []string{orgID})
	if err != nil {
		return nil, err
	}
	byID := map[string]CustomRole{}
	for _, r := range roles {
		byID[r.ID] = r
	}
	ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID})
	if err != nil {
		return nil, err
	}
	out := []OrgRoleGrant{}
	for _, t := range ts {
		id, ok := CustomRoleID(t.Relation)
		if !ok {
			continue
		}
		g := OrgRoleGrant{Tuple: t, Role: byID[id]}
		switch t.SubjectType {
		case TypeUser:
			if u, err := s.St.UserByID(ctx, t.SubjectID); err == nil {
				g.Label = u.Email
			}
		case TypeTeam:
			if team, err := s.St.TeamByID(ctx, t.SubjectID); err == nil {
				g.Label = team.Slug
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// customOrgPerms is what custom organisation roles give userID in orgID,
// directly or through teams. A grant naming a missing role, another
// organisation's or a repository role gives nothing.
func (s *Service) customOrgPerms(ctx context.Context, orgID, userID string, teamIDs []string) (Perms, error) {
	held, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: TypeUser, SubjectID: userID})
	if err != nil {
		return 0, err
	}
	if len(teamIDs) > 0 {
		ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: TypeTeam, SubjectIDs: teamIDs, SubjectRelation: TeamRead})
		if err != nil {
			return 0, err
		}
		held = append(held, ts...)
	}
	ids := map[string]bool{}
	for _, t := range held {
		if id, ok := CustomRoleID(t.Relation); ok {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	roles, err := s.St.CustomRoles(ctx, []string{orgID})
	if err != nil {
		return 0, err
	}
	var out Perms
	for _, r := range roles {
		if ids[r.ID] && r.OrgID == orgID && r.Resource == ResourceOrganisation {
			out |= r.Perms()
		}
	}
	return out, nil
}

// keyBaseline is what every key keeps whatever its scope: being a member
// and registering the runtime session it works in.
var keyBaseline = PermsOf(OrgMembership, OrgSessionsRead, OrgSessionsWrite)

// KeyLimit is the most p may do: everything for a session, a key's scope
// (with keyBaseline) for a scoped key. The key is read again every time,
// so a revoked key or a changed scope never lingers.
func (s *Service) KeyLimit(ctx context.Context, p Principal) (Perms, error) {
	if p.Via != "key" {
		return AllPerms(), nil
	}
	keys, err := s.St.ListAPIKeys(ctx, p.User.ID)
	if err != nil {
		return 0, err
	}
	for _, k := range keys {
		if k.ID == p.KeyID {
			if k.RevokedAt != nil || k.ExpiresAt != nil && !s.now().Before(*k.ExpiresAt) {
				return 0, ErrKeyRevoked
			}
			if k.Scope == nil {
				return AllPerms(), nil
			}
			return PermsOf(k.Scope...) | keyBaseline, nil
		}
	}
	return 0, ErrKeyRevoked
}

// PermsIn is p's organisation permissions in orgID: the built-in role
// (membership and teams), custom organisation roles, limited by a scoped
// key.
func (s *Service) PermsIn(ctx context.Context, p Principal, orgID string) (Perms, error) {
	r, decided, err := s.orgStanding(ctx, p, orgID)
	if err != nil {
		return 0, err
	}
	if s.Decider != nil || s.Remote != nil {
		// The decision service answered; AccessIn applied the key scope.
		return decided, nil
	}
	perms := r.Bundle(ResourceOrganisation)
	teams, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, SubjectType: TypeUser, SubjectID: p.User.ID})
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(teams))
	for i, t := range teams {
		ids[i] = t.ObjectID
	}
	custom, err := s.customOrgPerms(ctx, orgID, p.User.ID, ids)
	if err != nil {
		return 0, err
	}
	limit, err := s.KeyLimit(ctx, p)
	if err != nil {
		return 0, err
	}
	return (perms | custom) & limit, nil
}

// AccessFor is AccessOf limited by p's key scope: what an API key may
// reach is the key's scope intersected with what its user holds now.
func (s *Service) AccessFor(ctx context.Context, p Principal) (Access, error) {
	return s.accessFor(ctx, p, nil)
}

func (s *Service) accessFor(ctx context.Context, p Principal, q *AccessQuery) (Access, error) {
	a, err := s.accessScoped(ctx, p.User.ID, q)
	if err != nil {
		return a, err
	}
	limit, err := s.KeyLimit(ctx, p)
	if err != nil {
		return Access{}, err
	}
	return a.Within(limit), nil
}

// Within keeps only what limit allows; a repository without
// repository.code:read left drops out.
func (a Access) Within(limit Perms) Access {
	out := a
	out.OrgPerms = make(map[string]Perms, len(a.OrgPerms))
	for id, p := range a.OrgPerms {
		out.OrgPerms[id] = p & limit
	}
	out.Repos = make([]RepoAccess, 0, len(a.Repos))
	for _, r := range a.Repos {
		r.Perms &= limit
		if !r.Perms.Has(RepoCodeRead) {
			continue
		}
		r.Role = BuiltinRepoRole(r.Perms)
		out.Repos = append(out.Repos, r)
	}
	return out
}

// CheckKeyScope validates a requested key scope: known permissions that
// are not markers, each held by p in orgID now (organisation permissions
// there, repository permissions on at least one of its repositories).
// Nil asks for an unscoped key.
func (s *Service) CheckKeyScope(ctx context.Context, p Principal, orgID string, scope []Permission) ([]Permission, error) {
	if scope == nil {
		return nil, nil
	}
	if len(scope) == 0 || len(scope) > len(permAt) {
		return nil, fmt.Errorf("%w: a scoped key needs at least one permission", ErrInvalidInput)
	}
	var want Perms
	for _, x := range scope {
		if !x.Valid() || x.Marker() {
			return nil, fmt.Errorf("%w: unknown permission %q", ErrInvalidInput, x)
		}
		want |= PermsOf(x)
	}
	if !want.Only(ResourceRepository).Empty() {
		want |= PermsOf(RepoCodeRead)
	}
	held, err := s.PermsIn(ctx, p, orgID)
	if err != nil {
		return nil, err
	}
	a, err := s.AccessOf(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range a.Repos {
		if r.OrgID == orgID {
			held |= r.Perms
		}
	}
	if !held.Covers(want) {
		return nil, fmt.Errorf("%w: a key can only hold permissions you hold", ErrForbidden)
	}
	return want.List(), nil
}
