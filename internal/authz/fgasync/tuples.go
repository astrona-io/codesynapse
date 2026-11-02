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
			continue
		}
		level, ok := orgLevel[m.Role]
		if !ok {
			return nil, fmt.Errorf("%w: membership role %q", accounts.ErrInvalidInput, m.Role)
		}
		add(user, relMember, org)
		add(user, level, org)
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
			rel, ok := repoDirect[g.Role]
			if !ok {
				return nil, fmt.Errorf("%w: user grant %q", accounts.ErrInvalidInput, g.Role)
			}
			user, err := object(fga.TypeUser, g.SubjectID)
			if err != nil {
				return nil, err
			}
			add(user, rel, repo)
		}
		for _, g := range r.Teams {
			rel, ok := repoTeam[g.Role]
			if !ok {
				return nil, fmt.Errorf("%w: team grant %q", accounts.ErrInvalidInput, g.Role)
			}
			team, err := object(fga.TypeTeam, g.SubjectID)
			if err != nil {
				return nil, err
			}
			add(team+"#"+relTeamRead, rel, repo)
		}
	}
	return out, nil
}

// anchor reports whether t ties a team or repository to its organisation.
// Anchors are written first and deleted last, so every object with tuples
// has one and a later sync finds it.
func anchor(t fga.Tuple) bool { return t.Relation == relOrganisation }
