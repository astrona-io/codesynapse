package accountstest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// RunReBAC exercises relationship-based access through accounts.Service.
// It creates its own users, so it runs on any store state.
func RunReBAC(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	require.NoError(t, st.Migrate(ctx))
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, true)
	svc.Now = func() time.Time { return now }
	user := func(email string) (accounts.User, accounts.Principal) {
		u, _, err := svc.SignUp(ctx, email, email, pw)
		require.NoError(t, err)
		return u, accounts.Principal{User: u, Via: "session"}
	}
	owner, ownerP := user("rebac-owner@example.com")
	viewer, viewerP := user("rebac-viewer@example.com")
	guest, guestP := user("rebac-guest@example.com")
	teammate, _ := user("rebac-teammate@example.com")
	outsider, outsiderP := user("rebac-outsider@example.com")
	acme, err := svc.CreateOrg(ctx, ownerP, "ReBAC Acme", "")
	require.NoError(t, err)
	for _, m := range []struct {
		email string
		role  accounts.Role
	}{{viewer.Email, accounts.Viewer}, {guest.Email, accounts.Guest}, {teammate.Email, accounts.Guest}} {
		_, err := svc.AddMember(ctx, ownerP, acme.ID, m.email, m.role)
		require.NoError(t, err)
	}
	for _, g := range []string{"acme/api", "acme/web"} {
		require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: acme.ID, Graph: g, Remote: "https://github.com/" + g, Visibility: accounts.Private, AddedAt: now}))
	}
	reach := func(u accounts.User) map[string]accounts.Role {
		a, err := svc.AccessOf(ctx, u.ID)
		require.NoError(t, err)
		out := map[string]accounts.Role{}
		for _, r := range a.Repos {
			if r.OrgID == acme.ID {
				out[r.Graph] = r.Role
			}
		}
		return out
	}
	userSubject := func(u accounts.User) accounts.Tuple {
		return accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: u.ID}
	}

	// Organisation roles flow down; guests inherit nothing.
	assert.Equal(t, reach(viewer), map[string]accounts.Role{"acme/api": accounts.Viewer, "acme/web": accounts.Viewer})
	assert.Equal(t, reach(owner)["acme/api"], accounts.Admin)
	assert.Equal(t, len(reach(guest)), 0, "a guest sees only what is granted")

	// A direct grant to a guest.
	_, err = svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/web", userSubject(guest), accounts.Viewer)
	require.NoError(t, err)
	assert.Equal(t, reach(guest), map[string]accounts.Role{"acme/web": accounts.Viewer})

	// Teams: a role in the organisation flows down to every repository.
	platform, err := svc.CreateTeam(ctx, ownerP, acme.ID, "Platform")
	require.NoError(t, err)
	_, err = svc.SetTeamMember(ctx, ownerP, platform.ID, teammate.Email, accounts.RelMember)
	require.NoError(t, err)
	require.NoError(t, svc.SetTeamRole(ctx, ownerP, platform.ID, accounts.Member))
	assert.Equal(t, reach(teammate), map[string]accounts.Role{"acme/api": accounts.Member, "acme/web": accounts.Member})
	r, err := svc.RoleIn(ctx, accounts.Principal{User: teammate, Via: "session"}, acme.ID)
	require.NoError(t, err)
	assert.Equal(t, r, accounts.Member, "a team role counts as the member's organisation role")
	_, err = svc.SetTeamMember(ctx, ownerP, platform.ID, outsider.Email, accounts.RelMember)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "teams hold members of their organisation")

	// A team grant on one repository.
	contractors, err := svc.CreateTeam(ctx, ownerP, acme.ID, "Contractors")
	require.NoError(t, err)
	_, err = svc.SetTeamMember(ctx, ownerP, contractors.ID, guest.Email, accounts.RelMember)
	require.NoError(t, err)
	_, err = svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/api", accounts.Tuple{SubjectType: accounts.TypeTeam, SubjectID: contractors.ID, SubjectRelation: accounts.RelMember}, accounts.Member)
	require.NoError(t, err)
	assert.Equal(t, reach(guest), map[string]accounts.Role{"acme/api": accounts.Member, "acme/web": accounts.Viewer})

	// Repository admins grant inside the organisation, not outside it.
	_, err = svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/api", userSubject(viewer), accounts.Admin)
	require.NoError(t, err)
	_, err = svc.SetRepoGrant(ctx, viewerP, acme.ID, "acme/api", userSubject(outsider), accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "only organisation admins share outside")
	_, err = svc.SetRepoGrant(ctx, guestP, acme.ID, "acme/api", userSubject(outsider), accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "editors do not grant")

	// Sharing across organisations is read-only.
	_, err = svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/web", userSubject(outsider), accounts.Member)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "shares are read-only")
	g, err := svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/web", userSubject(outsider), accounts.Viewer)
	require.NoError(t, err)
	assert.True(t, g.External)
	assert.Equal(t, reach(outsider), map[string]accounts.Role{"acme/web": accounts.Viewer})
	_, err = svc.SetRepoGrant(ctx, accounts.Principal{User: owner, Via: "key", OrgID: acme.ID}, acme.ID, "acme/web", userSubject(guest), accounts.Admin)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "API keys do not change access")
	_, err = svc.SetRepoGrant(ctx, ownerP, acme.ID, "acme/nope", userSubject(guest), accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	grants, err := svc.RepoGrants(ctx, ownerP, acme.ID, "acme/web")
	require.NoError(t, err)
	labels := []string{}
	for _, g := range grants {
		labels = append(labels, g.Label)
	}
	assert.True(t, slices.Contains(labels, outsider.Email) && slices.Contains(labels, guest.Email), "grants name their subjects")
	_, err = svc.RepoGrants(ctx, outsiderP, acme.ID, "acme/web")
	assert.Error(t, err, "outsiders do not see who else has access")

	// Lost private access hides the entry from everyone who reached it.
	require.NoError(t, st.SetRepositoryAccess(ctx, acme.ID, "acme/web", accounts.AccessLost, now))
	assert.Equal(t, len(reach(outsider)), 0)
	_, hasWeb := reach(guest)["acme/web"]
	assert.False(t, hasWeb)
	require.NoError(t, st.SetRepositoryAccess(ctx, acme.ID, "acme/web", accounts.AccessOK, now))

	// Leaving the organisation drops team places and grants there.
	require.NoError(t, svc.RemoveMember(ctx, ownerP, acme.ID, guest.ID))
	assert.Equal(t, len(reach(guest)), 0)
	members, err := svc.TeamMembers(ctx, ownerP, contractors.ID)
	require.NoError(t, err)
	assert.Len(t, members, 0)

	// Deleting a team removes what it granted.
	require.NoError(t, svc.DeleteTeam(ctx, ownerP, platform.ID))
	assert.Equal(t, len(reach(teammate)), 0, "the team's role is gone; the guest membership remains")

	// Every grant change is audited.
	evs, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: acme.ID, Limit: 200})
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, e := range evs {
		actions[e.Action] = true
	}
	for _, a := range []string{"team.create", "team.member.set", "team.role.set", "repo.grant.set", "repo.share", "team.delete"} {
		assert.True(t, actions[a], "audited: "+a)
	}
}
