package accounts

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPermsOf(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []Permission
		has, lack []Permission
	}{
		{"full includes lower levels", []Permission{RepoSecurityFull}, []Permission{RepoSecurityRead, RepoSecurityWrite, RepoSecurityFull}, []Permission{RepoCodeRead}},
		{"areas stay apart", []Permission{RepoReviewsWrite}, []Permission{RepoReviewsRead}, []Permission{RepoReviewsFull, RepoSecurityRead}},
		{"markers are themselves only", []Permission{OrgOwnerRules}, []Permission{OrgOwnerRules}, []Permission{OrgMembership}},
		{"unknown is ignored", []Permission{"repository.code:delete"}, nil, []Permission{RepoCodeRead}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := PermsOf(tc.in...)
			for _, p := range tc.has {
				assert.True(t, s.Has(p), p)
			}
			for _, p := range tc.lack {
				assert.False(t, s.Has(p), p)
			}
		})
	}
}

func TestBuiltinBundlesMatchRoles(t *testing.T) {
	for _, r := range []Role{Guest, Read, Write, Full, Owner} {
		for _, res := range []string{ResourceRepository, ResourceOrganisation} {
			b := r.Bundle(res)
			for _, p := range AllPermissions() {
				if p.Resource() != res {
					assert.False(t, b.Has(p), r, p)
					continue
				}
				assert.Equal(t, b.Has(p), r.Has(p), r, p)
			}
		}
	}
	assert.Equal(t, BuiltinRepoRole(Full.Bundle(ResourceRepository)), Full)
	assert.Equal(t, BuiltinRepoRole(Read.Bundle(ResourceRepository)), Read)
	assert.Equal(t, BuiltinRepoRole(PermsOf(RepoCodeRead)), Role(""), "less than read is no built-in role")
}

func TestPermsSetOperations(t *testing.T) {
	a, b := PermsOf(RepoCodeRead, RepoSecurityWrite), PermsOf(RepoSecurityRead, RepoReviewsRead)
	assert.Equal(t, a.Intersect(b), PermsOf(RepoSecurityRead))
	assert.True(t, a.Union(b).Covers(a))
	assert.False(t, a.Covers(b))
	assert.Equal(t, PermsOf(RepoCodeRead, OrgAuditRead).Only(ResourceOrganisation), PermsOf(OrgAuditRead))
	assert.True(t, Perms(0).Empty())
	assert.Equal(t, a.Len(), 3)
}

func TestPermsJSON(t *testing.T) {
	in := PermsOf(RepoSecurityWrite)
	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Equal(t, string(data), `["repository.security:read","repository.security:write"]`)
	var out Perms
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, out, in)
	assert.True(t, json.Unmarshal([]byte(`["repository.code:delete"]`), &out) != nil, "unknown names are refused")
}

func FuzzPermsJSON(f *testing.F) {
	f.Add([]byte(`["repository.code:read"]`))
	f.Add([]byte(`["organisation#owner","organisation.audit:read"]`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var p Perms
		if json.Unmarshal(data, &p) != nil {
			return
		}
		again, err := json.Marshal(p)
		require.NoError(t, err)
		var q Perms
		require.NoError(t, json.Unmarshal(again, &q))
		assert.Equal(t, q, p, "a decoded set survives a round trip")
		for _, perm := range p.List() {
			assert.True(t, perm.Valid(), perm)
		}
	})
}

func FuzzCheckCustomRole(f *testing.F) {
	f.Add("Security triager", "Triages", ResourceRepository, string(RepoSecurityWrite))
	f.Add("admin", "", ResourceOrganisation, string(OrgAuditRead))
	f.Add("a:b", "\x00", "team", "x")
	f.Fuzz(func(t *testing.T, name, desc, resource, perm string) {
		r, err := checkCustomRole(CustomRoleInput{Name: name, Description: desc, Resource: resource, Permissions: []Permission{Permission(perm)}})
		if err != nil {
			return
		}
		assert.True(t, r.Resource == ResourceRepository || r.Resource == ResourceOrganisation)
		assert.True(t, len([]rune(r.Name)) <= MaxCustomRoleName && r.Name != "")
		for _, p := range r.Permissions {
			assert.True(t, p.Valid() && !p.Marker() && p.Resource() == r.Resource, p)
		}
	})
}
