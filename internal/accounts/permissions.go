package accounts

import (
	"slices"
	"strings"
)

// Permission is one fine-grained right, written resource.area:level
// (docs/plans/OPENFGA_PLAN.md, "Fine-grained permissions"). Routes, MCP
// tools and service methods check a permission, never a role; the built-in
// roles are bundles of permissions.
//
// A level includes the lower levels of the same area (full > write > read);
// areas never include each other. Two markers, organisation#member and
// organisation#owner, are relations rather than levels: every member holds
// the first, owners the second.
type Permission string

// Levels, weakest first.
const (
	LevelRead  = "read"
	LevelWrite = "write"
	LevelFull  = "full"
)

// Resources a permission applies to.
const (
	ResourceRepository   = "repository"
	ResourceOrganisation = "organisation"
)

// Repository permissions.
const (
	RepoCodeRead          Permission = "repository.code:read"
	RepoSecurityRead      Permission = "repository.security:read"
	RepoSecurityWrite     Permission = "repository.security:write"
	RepoSecurityFull      Permission = "repository.security:full"
	RepoReviewsRead       Permission = "repository.reviews:read"
	RepoReviewsWrite      Permission = "repository.reviews:write"
	RepoReviewsFull       Permission = "repository.reviews:full"
	RepoFixesWrite        Permission = "repository.fixes:write"
	RepoMemoryRead        Permission = "repository.memory:read"
	RepoMemoryWrite       Permission = "repository.memory:write"
	RepoMemoryFull        Permission = "repository.memory:full"
	RepoArchitectureRead  Permission = "repository.architecture:read"
	RepoArchitectureWrite Permission = "repository.architecture:write"
	RepoArchitectureFull  Permission = "repository.architecture:full"
	RepoRuntimeRead       Permission = "repository.runtime:read"
	RepoRuntimeWrite      Permission = "repository.runtime:write"
	RepoAIWrite           Permission = "repository.ai:write"
	RepoSettingsRead      Permission = "repository.settings:read"
	RepoSettingsFull      Permission = "repository.settings:full"
	RepoSyncWrite         Permission = "repository.sync:write"
	RepoAccessRead        Permission = "repository.access:read"
	RepoAccessFull        Permission = "repository.access:full"
	RepoEntryFull         Permission = "repository.entry:full"
)

// Organisation permissions.
const (
	OrgMembersRead       Permission = "organisation.members:read"
	OrgMembersWrite      Permission = "organisation.members:write"
	OrgMembersFull       Permission = "organisation.members:full"
	OrgTeamsRead         Permission = "organisation.teams:read"
	OrgTeamsFull         Permission = "organisation.teams:full"
	OrgRepositoriesRead  Permission = "organisation.repositories:read"
	OrgRepositoriesWrite Permission = "organisation.repositories:write"
	OrgRepositoriesFull  Permission = "organisation.repositories:full"
	OrgWatchesRead       Permission = "organisation.watches:read"
	OrgWatchesWrite      Permission = "organisation.watches:write"
	OrgWatchesFull       Permission = "organisation.watches:full"
	OrgGitTokensRead     Permission = "organisation.git_tokens:read"
	OrgGitTokensFull     Permission = "organisation.git_tokens:full"
	OrgSSORead           Permission = "organisation.sso:read"
	OrgSSOFull           Permission = "organisation.sso:full"
	OrgGatewayRead       Permission = "organisation.gateway:read"
	OrgGatewayWrite      Permission = "organisation.gateway:write"
	OrgGatewayFull       Permission = "organisation.gateway:full"
	OrgPromptLogRead     Permission = "organisation.prompt_log:read"
	OrgPromptLogFull     Permission = "organisation.prompt_log:full"
	OrgAuditRead         Permission = "organisation.audit:read"
	OrgRolesRead         Permission = "organisation.roles:read"
	OrgRolesFull         Permission = "organisation.roles:full"
	OrgSessionsRead      Permission = "organisation.sessions:read"
	OrgSessionsWrite     Permission = "organisation.sessions:write"
	OrgAgentSessionsRead Permission = "organisation.agent_sessions:read"
	OrgMembership        Permission = "organisation#member"
	OrgOwnerRules        Permission = "organisation#owner"
)

// permissionFloor is the weakest built-in role holding each permission:
// a role holds a permission when it is at least that strong. Keeping the
// bundles as floors on today's role order is what guarantees that the
// permission checks allow exactly what the role checks allowed before
// (TestBuiltInRolesKeepAccess in the API and MCP packages).
var permissionFloor = map[Permission]Role{
	RepoCodeRead:          Viewer,
	RepoSecurityRead:      Viewer,
	RepoSecurityWrite:     Member,
	RepoSecurityFull:      Admin,
	RepoReviewsRead:       Viewer,
	RepoReviewsWrite:      Member,
	RepoReviewsFull:       Admin,
	RepoFixesWrite:        Member,
	RepoMemoryRead:        Viewer,
	RepoMemoryWrite:       Member,
	RepoMemoryFull:        Member,
	RepoArchitectureRead:  Viewer,
	RepoArchitectureWrite: Member,
	RepoArchitectureFull:  Admin,
	RepoRuntimeRead:       Viewer,
	RepoRuntimeWrite:      Member,
	RepoAIWrite:           Member,
	RepoSettingsRead:      Viewer,
	RepoSettingsFull:      Admin,
	RepoSyncWrite:         Admin,
	RepoAccessRead:        Admin,
	RepoAccessFull:        Admin,
	RepoEntryFull:         Admin,

	OrgMembership:        Guest,
	OrgSessionsRead:      Guest,
	OrgSessionsWrite:     Guest,
	OrgMembersRead:       Viewer,
	OrgTeamsRead:         Viewer,
	OrgRepositoriesRead:  Viewer,
	OrgWatchesRead:       Viewer,
	OrgRolesRead:         Viewer,
	OrgRepositoriesWrite: Member,
	OrgWatchesWrite:      Member,
	OrgGatewayRead:       Member,
	OrgGatewayWrite:      Member,
	OrgGitTokensRead:     Member,
	OrgAgentSessionsRead: Member,
	OrgMembersWrite:      Admin,
	OrgMembersFull:       Admin,
	OrgTeamsFull:         Admin,
	OrgRepositoriesFull:  Admin,
	OrgWatchesFull:       Admin,
	OrgGitTokensFull:     Admin,
	OrgSSORead:           Admin,
	OrgSSOFull:           Admin,
	OrgGatewayFull:       Admin,
	OrgPromptLogRead:     Admin,
	OrgPromptLogFull:     Admin,
	OrgAuditRead:         Admin,
	OrgRolesFull:         Admin,
	OrgOwnerRules:        Owner,
}

// Valid reports whether p is a known permission.
func (p Permission) Valid() bool { _, ok := permissionFloor[p]; return ok }

// Resource is "repository" or "organisation".
func (p Permission) Resource() string {
	s := string(p)
	if i := strings.IndexAny(s, ".#"); i >= 0 {
		return s[:i]
	}
	return ""
}

// Area is the part between the resource and the level ("" for a marker).
func (p Permission) Area() string {
	s := string(p)
	dot, colon := strings.Index(s, "."), strings.LastIndex(s, ":")
	if dot < 0 || colon < dot {
		return ""
	}
	return s[dot+1 : colon]
}

// Level is read, write or full ("" for a marker).
func (p Permission) Level() string {
	if p.Marker() {
		return ""
	}
	_, level, _ := strings.Cut(string(p), ":")
	return level
}

// Marker reports whether p is the organisation#member or #owner relation.
func (p Permission) Marker() bool { return strings.Contains(string(p), "#") }

// Floor is the weakest built-in role that holds p ("" for an unknown one).
func (p Permission) Floor() Role { return permissionFloor[p] }

// Has reports whether the built-in role r holds p. Repository permissions
// apply to a repository role (viewer, member as editor, admin), the others
// to an organisation role. Unknown permissions are never held.
func (r Role) Has(p Permission) bool {
	least, ok := permissionFloor[p]
	return ok && r.AtLeast(least)
}

// Permissions lists what r holds on resource, sorted.
func (r Role) Permissions(resource string) []Permission {
	out := []Permission{}
	for p := range permissionFloor {
		if p.Resource() == resource && r.Has(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// AllPermissions lists every known permission, sorted.
func AllPermissions() []Permission {
	out := make([]Permission, 0, len(permissionFloor))
	for p := range permissionFloor {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
