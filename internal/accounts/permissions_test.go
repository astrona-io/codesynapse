package accounts

import (
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestPermissionNames(t *testing.T) {
	levels := map[string]int{LevelRead: 0, LevelWrite: 1, LevelFull: 2}
	for _, p := range AllPermissions() {
		assert.True(t, p.Resource() == ResourceRepository || p.Resource() == ResourceOrganisation, string(p))
		if p.Marker() {
			assert.Equal(t, p.Resource(), ResourceOrganisation, string(p))
			continue
		}
		_, ok := levels[p.Level()]
		assert.True(t, ok, string(p)+": level")
		assert.True(t, p.Area() != "", string(p)+": area")
		assert.Equal(t, string(p), p.Resource()+"."+p.Area()+":"+p.Level())
	}
	assert.False(t, Permission("repository.code:delete").Valid())
	assert.False(t, Admin.Has("repository.code:delete"), "unknown permissions are never held")
	assert.False(t, Role("").Has(RepoCodeRead))
}

// TestLevelsIncludeLowerLevels keeps a higher level of an area at least as
// hard to get as a lower one, so holding full always means holding write
// and read of the same area.
func TestLevelsIncludeLowerLevels(t *testing.T) {
	order := []string{LevelRead, LevelWrite, LevelFull}
	for _, p := range AllPermissions() {
		if p.Marker() {
			continue
		}
		for _, lower := range order[:slices.Index(order, p.Level())] {
			q := Permission(p.Resource() + "." + p.Area() + ":" + lower)
			if !q.Valid() {
				continue
			}
			for _, r := range []Role{Guest, Viewer, Member, Admin, Owner} {
				if r.Has(p) {
					assert.True(t, r.Has(q), string(r)+" holds "+string(p)+" but not "+string(q))
				}
			}
		}
	}
}

// TestBuiltInBundles pins the bundles of docs/plans/OPENFGA_PLAN.md
// ("Built-in roles"), with the changes that keep today's access.
func TestBuiltInBundles(t *testing.T) {
	var repoRead []Permission
	for _, p := range AllPermissions() {
		if p.Resource() == ResourceRepository && p.Level() == LevelRead && p != RepoAccessRead {
			repoRead = append(repoRead, p)
		}
	}
	assert.Equal(t, Viewer.Permissions(ResourceRepository), repoRead)
	write := append(slices.Clone(repoRead), RepoSecurityWrite, RepoReviewsWrite, RepoFixesWrite, RepoMemoryWrite,
		RepoMemoryFull, RepoArchitectureWrite, RepoRuntimeWrite, RepoAIWrite)
	slices.Sort(write)
	assert.Equal(t, Member.Permissions(ResourceRepository), write)
	var all []Permission
	for _, p := range AllPermissions() {
		if p.Resource() == ResourceRepository {
			all = append(all, p)
		}
	}
	assert.Equal(t, Admin.Permissions(ResourceRepository), all)

	guest := []Permission{OrgMembership, OrgSessionsRead, OrgSessionsWrite}
	slices.Sort(guest)
	assert.Equal(t, Guest.Permissions(ResourceOrganisation), guest)
	read := append(slices.Clone(guest), OrgMembersRead, OrgTeamsRead, OrgRepositoriesRead, OrgWatchesRead, OrgRolesRead)
	slices.Sort(read)
	assert.Equal(t, Viewer.Permissions(ResourceOrganisation), read)
	orgWrite := append(slices.Clone(read), OrgRepositoriesWrite, OrgWatchesWrite, OrgGatewayRead, OrgGatewayWrite,
		OrgGitTokensRead, OrgAgentSessionsRead)
	slices.Sort(orgWrite)
	assert.Equal(t, Member.Permissions(ResourceOrganisation), orgWrite)
	full := slices.DeleteFunc(slices.Clone(AllPermissions()), func(p Permission) bool {
		return p.Resource() != ResourceOrganisation || p == OrgOwnerRules
	})
	assert.Equal(t, Admin.Permissions(ResourceOrganisation), full)
	ownerAll := slices.DeleteFunc(AllPermissions(), func(p Permission) bool { return p.Resource() != ResourceOrganisation })
	assert.Equal(t, Owner.Permissions(ResourceOrganisation), ownerAll)
}
