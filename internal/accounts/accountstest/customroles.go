package accountstest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunCustomRoles covers custom roles and scoped keys
// (docs/plans/OPENFGA_PLAN.md, phase 3): who may manage roles, no
// escalation, grants on repositories and the organisation, deleting a role
// in use, broken grants failing closed, and keys limited to their scope.
func RunCustomRoles(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	require.NoError(t, st.Migrate(ctx))
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, true)
	svc.Now = func() time.Time { return now }
	user := func(email string) (accounts.User, accounts.Principal) {
		u, _, err := svc.SignUp(ctx, email, email, pw)
		require.NoError(t, err)
		return u, sessionFor(ctx, t, svc, u)
	}
	owner, ownerP := user("roles-owner@example.com")
	reader, readerP := user("roles-reader@example.com")
	lead, leadP := user("roles-lead@example.com")
	org, err := svc.CreateOrg(ctx, ownerP, "Roles Acme", "")
	require.NoError(t, err)
	require.NoError(t, Join(ctx, t, svc, ownerP, org.ID, reader, accounts.Read))
	require.NoError(t, Join(ctx, t, svc, ownerP, org.ID, lead, accounts.Read))
	graph := "roles/api"
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, AddedBy: owner.ID, Visibility: accounts.Private, AddedAt: now}))
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: owner.ID}, accounts.Full)
	require.NoError(t, err)
	repoPerms := func(u accounts.User) accounts.Perms {
		a, err := svc.AccessOf(ctx, u.ID)
		require.NoError(t, err)
		for _, r := range a.Repos {
			if r.OrgID == org.ID && r.Graph == graph {
				return r.Perms
			}
		}
		return 0
	}
	triager := accounts.CustomRoleInput{Name: "Security triager", Description: "Triages findings", Resource: accounts.ResourceRepository,
		Permissions: []accounts.Permission{accounts.RepoSecurityWrite}}

	t.Run("only signed-in role managers", func(t *testing.T) {
		_, err := svc.CreateCustomRole(ctx, readerP, org.ID, triager)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "read members cannot manage roles")
		keyed := ownerP
		keyed.Via, keyed.KeyID = "key", "any"
		_, err = svc.CreateCustomRole(ctx, keyed, org.ID, triager)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "API keys never manage roles")
		rs, err := svc.CustomRoles(ctx, readerP, org.ID)
		require.NoError(t, err, "members may read roles")
		assert.Len(t, rs, 0)
	})

	t.Run("input is validated", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   accounts.CustomRoleInput
		}{
			{"reserved name", accounts.CustomRoleInput{Name: "Admin", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{accounts.RepoCodeRead}}},
			{"empty name", accounts.CustomRoleInput{Name: " ", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{accounts.RepoCodeRead}}},
			{"separator in name", accounts.CustomRoleInput{Name: "a:b", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{accounts.RepoCodeRead}}},
			{"unknown permission", accounts.CustomRoleInput{Name: "x", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{"repository.code:delete"}}},
			{"other resource", accounts.CustomRoleInput{Name: "x", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{accounts.OrgAuditRead}}},
			{"marker", accounts.CustomRoleInput{Name: "x", Resource: accounts.ResourceOrganisation, Permissions: []accounts.Permission{accounts.OrgOwnerRules}}},
			{"unknown resource", accounts.CustomRoleInput{Name: "x", Resource: "team", Permissions: []accounts.Permission{accounts.RepoCodeRead}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := svc.CreateCustomRole(ctx, ownerP, org.ID, tc.in)
				assert.True(t, errors.Is(err, accounts.ErrInvalidInput), err)
			})
		}
	})

	role, err := svc.CreateCustomRole(ctx, ownerP, org.ID, triager)
	require.NoError(t, err)
	assert.True(t, role.Perms().Has(accounts.RepoSecurityRead), "levels include the lower ones")
	assert.True(t, role.Perms().Has(accounts.RepoCodeRead), "repository roles always read code")
	assert.False(t, role.Perms().Has(accounts.RepoReviewsWrite))
	_, err = svc.CreateCustomRole(ctx, ownerP, org.ID, accounts.CustomRoleInput{Name: "SECURITY TRIAGER", Resource: accounts.ResourceRepository, Permissions: []accounts.Permission{accounts.RepoCodeRead}})
	assert.True(t, errors.Is(err, accounts.ErrExists), "names are unique per organisation, case-insensitively")

	t.Run("repository grant", func(t *testing.T) {
		assert.True(t, repoPerms(reader).Empty(), "new organisations default to NONE")
		sub := accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: reader.ID}
		g, err := svc.SetRepoGrant(ctx, ownerP, org.ID, graph, sub, accounts.Role(accounts.CustomRelation(role.ID)))
		require.NoError(t, err)
		require.NotNil(t, g.CustomRole)
		assert.Equal(t, g.CustomRole.Name, role.Name)
		got := repoPerms(reader)
		assert.True(t, got.Has(accounts.RepoSecurityWrite))
		assert.False(t, got.Has(accounts.RepoReviewsWrite), "a custom role holds only its permissions")
		_, err = svc.SetRepoGrant(ctx, readerP, org.ID, graph, sub, accounts.Role(accounts.CustomRelation(role.ID)))
		assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound), "only repository admins grant")
		err = svc.DeleteCustomRole(ctx, ownerP, org.ID, role.ID)
		assert.True(t, errors.Is(err, accounts.ErrRoleInUse), "a granted role stays")
		assert.True(t, errors.Is(err, accounts.ErrExists), "answers 409")
		p, err := svc.OrgPolicy(ctx, org.ID)
		require.NoError(t, err)
		var mirrored *accounts.PolicyGrant
		for _, r := range p.Repos {
			for i, g := range r.Users {
				if r.Graph == graph && g.SubjectID == reader.ID {
					mirrored = &r.Users[i]
				}
			}
		}
		require.NotNil(t, mirrored, "the policy OpenFGA mirrors holds the custom grant")
		assert.True(t, mirrored.Custom && mirrored.Role == "")
		assert.Equal(t, mirrored.Perms, role.Perms(), "as the role's permissions")
	})

	t.Run("broken grant fails closed", func(t *testing.T) {
		u, _ := user("roles-broken@example.com")
		require.NoError(t, Join(ctx, t, svc, ownerP, org.ID, u, accounts.Read))
		require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, org.ID, accounts.DefaultRepoReadOnly))
		assert.True(t, repoPerms(u).Has(accounts.RepoCodeRead), "the default applies")
		require.NoError(t, st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(org.ID, graph),
			Relation: accounts.CustomRelation("missing-role"), SubjectType: accounts.TypeUser, SubjectID: u.ID, CreatedAt: now}))
		assert.True(t, repoPerms(u).Empty(), "a grant naming a missing role gives nothing, not the default")
		require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, org.ID, accounts.DefaultRepoNone))
	})

	t.Run("organisation grant and escalation", func(t *testing.T) {
		manager, err := svc.CreateCustomRole(ctx, ownerP, org.ID, accounts.CustomRoleInput{Name: "Role manager", Resource: accounts.ResourceOrganisation,
			Permissions: []accounts.Permission{accounts.OrgRolesFull}})
		require.NoError(t, err)
		before, err := svc.PermsIn(ctx, leadP, org.ID)
		require.NoError(t, err)
		assert.False(t, before.Has(accounts.OrgRolesFull))
		_, err = svc.SetOrgRoleGrant(ctx, readerP, org.ID, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: lead.ID}, manager.ID)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "read members cannot grant")
		_, err = svc.SetOrgRoleGrant(ctx, ownerP, org.ID, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: lead.ID}, role.ID)
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "a repository role is not an organisation role")
		_, err = svc.SetOrgRoleGrant(ctx, ownerP, org.ID, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: lead.ID}, manager.ID)
		require.NoError(t, err)
		after, err := svc.PermsIn(ctx, leadP, org.ID)
		require.NoError(t, err)
		assert.True(t, after.Has(accounts.OrgRolesFull), "custom organisation roles add to the built-in one")
		assert.True(t, after.Has(accounts.OrgMembersRead), "the built-in role stays")
		gs, err := svc.OrgRoleGrants(ctx, ownerP, org.ID)
		require.NoError(t, err)
		assert.Len(t, gs, 1)

		_, err = svc.CreateCustomRole(ctx, leadP, org.ID, accounts.CustomRoleInput{Name: "Auditor", Resource: accounts.ResourceOrganisation,
			Permissions: []accounts.Permission{accounts.OrgAuditRead}})
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "no one creates a role holding more than they do")
		_, err = svc.CreateCustomRole(ctx, leadP, org.ID, accounts.CustomRoleInput{Name: "Fixer", Resource: accounts.ResourceRepository,
			Permissions: []accounts.Permission{accounts.RepoFixesWrite}})
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "nor a repository role beyond their repositories")
		_, err = svc.CreateCustomRole(ctx, leadP, org.ID, accounts.CustomRoleInput{Name: "Member reader", Resource: accounts.ResourceOrganisation,
			Permissions: []accounts.Permission{accounts.OrgMembersRead}})
		require.NoError(t, err, "within what they hold")
		_, err = svc.UpdateCustomRole(ctx, leadP, org.ID, role.ID, triager)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "nor change a role reaching further than they do")

		require.NoError(t, svc.RemoveOrgRoleGrant(ctx, ownerP, org.ID, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: lead.ID}))
		after, err = svc.PermsIn(ctx, leadP, org.ID)
		require.NoError(t, err)
		assert.False(t, after.Has(accounts.OrgRolesFull))
		require.NoError(t, svc.DeleteCustomRole(ctx, ownerP, org.ID, manager.ID), "an ungranted role can go")
	})

	t.Run("update reaches every holder", func(t *testing.T) {
		in := triager
		in.Permissions = []accounts.Permission{accounts.RepoSecurityWrite, accounts.RepoReviewsWrite}
		before, err := st.OrgByID(ctx, org.ID)
		require.NoError(t, err)
		updated, err := svc.UpdateCustomRole(ctx, ownerP, org.ID, role.ID, in)
		require.NoError(t, err)
		after, err := st.OrgByID(ctx, org.ID)
		require.NoError(t, err)
		assert.True(t, after.PolicyRevision > before.PolicyRevision, "changing a role changes access, so the policy revision moves")
		assert.Equal(t, updated.Resource, accounts.ResourceRepository)
		assert.True(t, repoPerms(reader).Has(accounts.RepoReviewsWrite))
		in.Resource = accounts.ResourceOrganisation
		_, err = svc.UpdateCustomRole(ctx, ownerP, org.ID, role.ID, in)
		assert.True(t, err != nil, "a role keeps its resource")
	})

	t.Run("scoped keys", func(t *testing.T) {
		_, _, err := svc.CreateKey(ctx, readerP, org.ID, "too wide", 0, []accounts.Permission{accounts.RepoFixesWrite})
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "a key holds only what its user holds")
		_, _, err = svc.CreateKey(ctx, readerP, org.ID, "unknown", 0, []accounts.Permission{"repository.code:delete"})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		token, key, err := svc.CreateKey(ctx, readerP, org.ID, "triage only", 0, []accounts.Permission{accounts.RepoSecurityRead})
		require.NoError(t, err)
		assert.True(t, slices.Contains(key.Scope, accounts.RepoCodeRead), "repository scopes read code")
		kp, err := svc.AuthenticateKey(ctx, token)
		require.NoError(t, err)
		a, err := svc.AccessFor(ctx, kp)
		require.NoError(t, err)
		require.Len(t, a.Repos, 1)
		assert.True(t, a.Repos[0].Perms.Has(accounts.RepoSecurityRead))
		assert.False(t, a.Repos[0].Perms.Has(accounts.RepoSecurityWrite), "the scope limits what the user holds")
		held, err := svc.PermsIn(ctx, kp, org.ID)
		require.NoError(t, err)
		assert.False(t, held.Has(accounts.OrgMembersRead), "organisation permissions outside the scope are dropped")
		assert.True(t, held.Has(accounts.OrgSessionsWrite), "every key may register its runtime session")
		assert.True(t, errors.Is(svc.Require(ctx, kp, org.ID, accounts.OrgMembersRead), accounts.ErrForbidden))

		// The scope never outlasts the user's own access.
		require.NoError(t, svc.RemoveRepoGrant(ctx, ownerP, org.ID, graph, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: reader.ID}))
		a, err = svc.AccessFor(ctx, kp)
		require.NoError(t, err)
		assert.Len(t, a.Repos, 0)

		token, key, err = svc.CreateKey(ctx, readerP, org.ID, "everything", 0, nil)
		require.NoError(t, err)
		assert.True(t, key.Scope == nil, "unscoped keys act as their user")
		kp, err = svc.AuthenticateKey(ctx, token)
		require.NoError(t, err)
		held, err = svc.PermsIn(ctx, kp, org.ID)
		require.NoError(t, err)
		assert.True(t, held.Has(accounts.OrgMembersRead))
		require.NoError(t, svc.RevokeKey(ctx, readerP, key.ID))
		_, err = svc.AccessFor(ctx, kp)
		assert.True(t, err != nil, "a revoked key reaches nothing")
	})

	require.NoError(t, svc.DeleteCustomRole(ctx, ownerP, org.ID, role.ID), "deleted once ungranted")

	t.Run("limit", func(t *testing.T) {
		rs, err := svc.CustomRoles(ctx, ownerP, org.ID)
		require.NoError(t, err)
		for i := len(rs); i < accounts.MaxCustomRoles; i++ {
			_, err := svc.CreateCustomRole(ctx, ownerP, org.ID, accounts.CustomRoleInput{Name: fmt.Sprintf("Role %02d", i), Resource: accounts.ResourceRepository,
				Permissions: []accounts.Permission{accounts.RepoCodeRead}})
			require.NoError(t, err)
		}
		_, err = svc.CreateCustomRole(ctx, ownerP, org.ID, accounts.CustomRoleInput{Name: "One too many", Resource: accounts.ResourceRepository,
			Permissions: []accounts.Permission{accounts.RepoCodeRead}})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
	})

	t.Run("audited", func(t *testing.T) {
		for _, action := range []string{"org.role.create", "org.role.update", "org.role.delete", "org.role.grant", "org.role.revoke"} {
			evs, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: action})
			require.NoError(t, err)
			assert.True(t, len(evs) > 0, action)
		}
	})
}
