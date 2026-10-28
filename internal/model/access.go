package model

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Access projections are separate from code nodes and never authorize a request.
const (
	KindOrganization     NodeKind = "Organization"
	KindUser             NodeKind = "User"
	KindTeam             NodeKind = "Team"
	KindAccessRepository NodeKind = "AccessRepository"
	EdgeMemberOf         EdgeKind = "MEMBER_OF"
	EdgeBelongsTo        EdgeKind = "BELONGS_TO"
	EdgeHasAccess        EdgeKind = "HAS_ACCESS"
	MaxAccessItems                = 5000
	MaxAccessTeams                = 128
)

// AccessUser is a tenant-local user enrolled in the organisation.
type AccessUser struct {
	ID              string
	Active          bool
	InheritsDefault bool
}

// AccessRepo names an organisation entry without an edge into the code graph.
type AccessRepo struct {
	ID, GraphID string
	Usable      bool
	// Personal entries are in a member's own space: the organisation default
	// gives no access to them. Omitted when false, so digests of snapshots
	// without personal entries stay as they were.
	Personal bool `json:",omitempty"`
}

// AccessMembership connects an enrolled user to a team in the same tenant.
type AccessMembership struct{ UserID, TeamID string }

// AccessGrant overrides the default for one repository. NONE is user-only.
type AccessGrant struct {
	SubjectKind             NodeKind
	SubjectID, RepoID, Role string
}

// AccessSnapshot is one complete policy revision, excluding credentials and PII.
type AccessSnapshot struct {
	TenantID string
	Revision int64
	Default  string
	Users    []AccessUser
	Teams    []string
	Repos    []AccessRepo
	Members  []AccessMembership
	Grants   []AccessGrant
}

// AccessDecision is an explanation to compare with authoritative accounts data.
type AccessDecision struct {
	GraphID string
	Rank    int
}

// AccessRank returns -1 for a malformed permission.
func AccessRank(role string) int {
	switch role {
	case "NONE":
		return 0
	case "READ":
		return 1
	case "WRITE":
		return 2
	case "ADMIN":
		return 3
	}
	return -1
}

// AccessID gives each revision disjoint vertices, including incomplete rebuilds.
func AccessID(tenant string, revision int64, kind NodeKind, id string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", tenant, revision, kind, id))
	return "access:" + hex.EncodeToString(h[:])
}

// Validate rejects incomplete, duplicate and cross-tenant relationships before writes.
func (s AccessSnapshot) Validate() error {
	bad := errors.New("invalid access snapshot")
	if s.TenantID == "" || s.Revision < 1 || (s.Default != "NONE" && s.Default != "READ_ONLY") {
		return bad
	}
	if len(s.Users)+len(s.Teams)+len(s.Repos)+len(s.Members)+len(s.Grants) > MaxAccessItems {
		return bad
	}
	users, teams, repos := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, u := range s.Users {
		if u.ID == "" || users[u.ID] {
			return bad
		}
		users[u.ID] = true
	}
	for _, t := range s.Teams {
		if t == "" || teams[t] {
			return bad
		}
		teams[t] = true
	}
	for _, r := range s.Repos {
		if r.ID == "" || r.GraphID == "" || repos[r.ID] {
			return bad
		}
		repos[r.ID] = true
	}
	seen, counts := map[AccessMembership]bool{}, map[string]int{}
	for _, m := range s.Members {
		if !users[m.UserID] || !teams[m.TeamID] || seen[m] {
			return bad
		}
		seen[m] = true
		counts[m.UserID]++
		if counts[m.UserID] > MaxAccessTeams {
			return bad
		}
	}
	grants := map[AccessGrant]bool{}
	for _, g := range s.Grants {
		rank := AccessRank(g.Role)
		if !repos[g.RepoID] || rank < 0 || (g.SubjectKind != KindUser && g.SubjectKind != KindTeam) {
			return bad
		}
		if g.SubjectKind == KindUser && !users[g.SubjectID] {
			return bad
		}
		if g.SubjectKind == KindTeam && (!teams[g.SubjectID] || rank == 0) {
			return bad
		}
		g.Role = ""
		if grants[g] {
			return bad
		}
		grants[g] = true
	}
	return nil
}

// Evaluate is the reference implementation used by the memory store and parity tests.
func (s AccessSnapshot) Evaluate(userID, repoID string) AccessDecision {
	if s.Validate() != nil {
		return AccessDecision{}
	}
	var user AccessUser
	for _, u := range s.Users {
		if u.ID == userID {
			user = u
			break
		}
	}
	if !user.Active {
		return AccessDecision{}
	}
	var repo AccessRepo
	for _, r := range s.Repos {
		if r.ID == repoID {
			repo = r
			break
		}
	}
	if !repo.Usable {
		return AccessDecision{}
	}
	result := AccessDecision{GraphID: repo.GraphID}
	if user.InheritsDefault && s.Default == "READ_ONLY" && !repo.Personal {
		result.Rank = 1
	}
	teams := map[string]bool{}
	for _, m := range s.Members {
		if m.UserID == userID {
			teams[m.TeamID] = true
		}
	}
	teamRank := -1
	for _, g := range s.Grants {
		if g.RepoID != repoID {
			continue
		}
		if g.SubjectKind == KindUser && g.SubjectID == userID {
			result.Rank = AccessRank(g.Role)
			return result
		}
		if g.SubjectKind == KindTeam && teams[g.SubjectID] {
			teamRank = max(teamRank, AccessRank(g.Role))
		}
	}
	if teamRank >= 0 {
		result.Rank = teamRank
	}
	return result
}

// Digest identifies a policy independently of the order in which rows were read.
func (s AccessSnapshot) Digest() string {
	s.Users, s.Teams, s.Repos = append([]AccessUser{}, s.Users...), append([]string{}, s.Teams...), append([]AccessRepo{}, s.Repos...)
	s.Members, s.Grants = append([]AccessMembership{}, s.Members...), append([]AccessGrant{}, s.Grants...)
	slices.SortFunc(s.Users, func(a, b AccessUser) int { return cmp.Compare(a.ID, b.ID) })
	slices.Sort(s.Teams)
	slices.SortFunc(s.Repos, func(a, b AccessRepo) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(s.Members, func(a, b AccessMembership) int {
		return cmp.Or(cmp.Compare(a.UserID, b.UserID), cmp.Compare(a.TeamID, b.TeamID))
	})
	slices.SortFunc(s.Grants, func(a, b AccessGrant) int {
		return cmp.Or(cmp.Compare(a.SubjectKind, b.SubjectKind), cmp.Compare(a.SubjectID, b.SubjectID), cmp.Compare(a.RepoID, b.RepoID), cmp.Compare(a.Role, b.Role))
	})
	data, _ := json.Marshal(s) //nolint:errcheck // only strings, integers, booleans and slices
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// AccessVertexID separates concurrent builds even if their revision was reused.
func AccessVertexID(tenant string, revision int64, digest string, kind NodeKind, id string) string {
	return AccessID(tenant, revision, kind, digest+":"+id)
}
