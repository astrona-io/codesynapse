// Package fgasync keeps OpenFGA's tuples in step with the access policy in
// PostgreSQL (docs/plans/OPENFGA_PLAN.md, phase 5). PostgreSQL stays the
// source of truth: for each organisation the sync computes every tuple the
// model needs, reads what OpenFGA holds, writes the difference and then
// records the policy revision OpenFGA accepted. Nothing asks OpenFGA for a
// decision yet; Fresh is the check phase 6 will put in front of it.
package fgasync

import (
	"fmt"
	"regexp"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
)

// Model relations. The policy's role names map to them here and nowhere
// else.
const (
	relOrganisation   = "organisation"
	relOrgSpace       = "org_space"
	relMember         = "member"
	relGuest          = "guest"
	relDefaultReaders = "default_readers"
	// Team relations; team grants name team#read as their subject.
	relTeamRead = accounts.TeamRead
	relTeamFull = accounts.TeamFull
)

// orgLevel is the organisation level a membership or team role holds.
// Guests hold none: they are only in guest.
var orgLevel = map[accounts.Role]string{
	accounts.Read:  "read",
	accounts.Write: "write",
	accounts.Full:  "full",
	accounts.Owner: "owner",
}

// repoDirect is the relation of a user's repository grant; Deny is the
// explicit deny.
var repoDirect = map[accounts.Role]string{
	accounts.Full:  "direct_full",
	accounts.Write: "direct_write",
	accounts.Read:  "direct_read",
	accounts.Deny:  "direct_deny",
}

// relDirectCustom marks a user's custom role grant on a repository; the
// role's permissions go in user_<permission> beside it.
const relDirectCustom = "direct_custom"

// relTeamCustom marks a team's custom role grant, with team_<permission>.
const relTeamCustom = "team_custom"

// repoTeam is the relation of a team's repository grant.
var repoTeam = map[accounts.Role]string{
	accounts.Full:  "team_full",
	accounts.Write: "team_write",
	accounts.Read:  "team_read",
}

// validID accepts the IDs accounts makes (24 hex characters) and keeps
// anything that could change a tuple's meaning (":", "#", spaces) out.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func object(typ, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("%w: %s ID", accounts.ErrInvalidInput, typ)
	}
	return typ + ":" + id, nil
}

// Desired returns every tuple OpenFGA must hold for p, in a stable order.
// Anything it cannot express is an error, so a sync never writes part of a
// policy.
func Desired(p accounts.OrgPolicy) ([]fga.Tuple, error) {
	org, err := object(fga.TypeOrganisation, p.Org.ID)
	if err != nil {
		return nil, err
	}
	var out []fga.Tuple
	add := func(user, relation, obj string) {
		out = append(out, fga.Tuple{User: user, Relation: relation, Object: obj})
	}
	if p.Org.DefaultRepoAccess == accounts.DefaultRepoReadOnly {
		add(org+"#"+relMember, relDefaultReaders, org)
	}
	for _, m := range p.Members {
		user, err := object(fga.TypeUser, m.UserID)
		if err != nil {
			return nil, err
		}
		if m.Role == accounts.Guest {
			add(user, relGuest, org)
		} else {
			level, ok := orgLevel[m.Role]
			if !ok {
				return nil, fmt.Errorf("%w: membership role %q", accounts.ErrInvalidInput, m.Role)
			}
			add(user, relMember, org)
			add(user, level, org)
		}
		if err := addPerms(add, user, "", org, m.Perms, accounts.ResourceOrganisation); err != nil {
			return nil, err
		}
	}
	for _, t := range p.Teams {
		team, err := object(fga.TypeTeam, t.ID)
		if err != nil {
			return nil, err
		}
		add(org, relOrganisation, team)
		// A guest role through a team holds no organisation level, like a
		// guest membership.
		if level, ok := orgLevel[t.Role]; ok {
			add(team+"#"+relTeamRead, level, org)
		}
		if err := addPerms(add, team+"#"+relTeamRead, "", org, t.Perms, accounts.ResourceOrganisation); err != nil {
			return nil, err
		}
		for _, m := range t.Members {
			user, err := object(fga.TypeUser, m.UserID)
			if err != nil {
				return nil, err
			}
			rel := relTeamRead
			if m.Maintainer {
				rel = relTeamFull
			}
			add(user, rel, team)
		}
	}
	for _, r := range p.Repos {
		repo := fga.TypeRepository + ":" + fga.RepositoryID(p.Org.ID, r.Graph)
		add(org, relOrganisation, repo)
		if !r.Personal {
			add(org, relOrgSpace, repo)
		}
		for _, g := range r.Users {
			user, err := object(fga.TypeUser, g.SubjectID)
			if err != nil {
				return nil, err
			}
			if g.Custom {
				// Also for a role that grants nothing: the grant still
				// replaces the user's team grants and the default.
				add(user, relDirectCustom, repo)
				if err := addPerms(add, user, "user_", repo, g.Perms, accounts.ResourceRepository); err != nil {
					return nil, err
				}
				continue
			}
			rel, ok := repoDirect[g.Role]
			if !ok {
				return nil, fmt.Errorf("%w: user grant %q", accounts.ErrInvalidInput, g.Role)
			}
			add(user, rel, repo)
		}
		for _, g := range r.Teams {
			team, err := object(fga.TypeTeam, g.SubjectID)
			if err != nil {
				return nil, err
			}
			if g.Custom {
				// The mark replaces the default, also for a role that grants
				// nothing, as AccessOf does.
				add(team+"#"+relTeamRead, relTeamCustom, repo)
				if err := addPerms(add, team+"#"+relTeamRead, "team_", repo, g.Perms, accounts.ResourceRepository); err != nil {
					return nil, err
				}
				continue
			}
			rel, ok := repoTeam[g.Role]
			if !ok {
				return nil, fmt.Errorf("%w: team grant %q", accounts.ErrInvalidInput, g.Role)
			}
			add(team+"#"+relTeamRead, rel, repo)
		}
	}
	return out, nil
}

// addPerms writes one tuple per permission a custom role grants, under
// prefix (user_ or team_ on a repository, none on the organisation). perms
// is already closed within each area (accounts.PermsOf), so each
// permission is written as it is. A permission of the other resource or a
// marker is an error: roles never hold one.
func addPerms(add func(user, relation, obj string), user, prefix, obj string, perms accounts.Perms, resource string) error {
	for _, p := range perms.List() {
		if p.Marker() || p.Resource() != resource {
			return fmt.Errorf("%w: %s in a %s role", accounts.ErrInvalidInput, p, resource)
		}
		add(user, prefix+Relation(p), obj)
	}
	return nil
}

// anchor reports whether t ties a team or repository to its organisation.
// Anchors are written first and deleted last, so every object with tuples
// has one and a later sync finds it.
func anchor(t fga.Tuple) bool { return t.Relation == relOrganisation }

// Relation is the model relation of a permission: area_level
// (repository.reviews:write is reviews_write on a repository), with the
// two markers mapped to the relations they are: organisation#member is
// active (guests included) and organisation#owner is owner. The model
// files are generated from the same mapping (TestModelFiles).
func Relation(p accounts.Permission) string {
	switch p {
	case accounts.OrgMembership:
		return "active"
	case accounts.OrgOwnerRules:
		return "owner"
	}
	if !p.Valid() {
		return ""
	}
	return p.Area() + "_" + p.Level()
}
