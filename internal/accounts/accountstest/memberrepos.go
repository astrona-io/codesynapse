package accountstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunMemberRepos covers the two spaces of an organisation: the setting that
// lets members add their own repositories (on by default, admins only,
// audited), and a member's own repository getting no organisation default.
func RunMemberRepos(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, true)
	svc.Now = func() time.Time { return now }
	user := func(email string) (accounts.User, accounts.Principal) {
		u, _, err := svc.SignUp(ctx, email, email, pw)
		require.NoError(t, err)
		hash := "space-" + u.ID
		require.NoError(t, st.CreateSession(ctx, accounts.Session{TokenHash: hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}))
		return u, accounts.Principal{User: u, Via: "session", SessionHash: hash}
	}
	owner, ownerP := user("space-owner@example.com")
	member, memberP := user("space-member@example.com")
	other, _ := user("space-other@example.com")
	org, err := svc.CreateOrg(ctx, ownerP, "Space Acme", "")
	require.NoError(t, err)
	for _, u := range []accounts.User{member, other} {
		require.NoError(t, Join(ctx, t, svc, ownerP, org.ID, u, accounts.Member))
	}

	on, err := svc.MemberReposSetting(ctx, memberP, org.ID)
	require.NoError(t, err)
	assert.True(t, on, "members may add their own until an admin says otherwise")
	assert.True(t, errors.Is(svc.SetMemberRepos(ctx, memberP, org.ID, false), accounts.ErrForbidden), "members cannot change it")
	key := accounts.Principal{User: owner, Via: "key", OrgID: org.ID}
	assert.True(t, errors.Is(svc.SetMemberRepos(ctx, key, org.ID, false), accounts.ErrForbidden), "API keys cannot change it")
	require.NoError(t, svc.SetMemberRepos(ctx, ownerP, org.ID, false))
	on, err = svc.MemberReposSetting(ctx, memberP, org.ID)
	require.NoError(t, err)
	assert.False(t, on)
	rows, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "org.member_repos.set", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the change is audited")
	assert.Equal(t, rows[0].Detail, "enabled=false")
	require.NoError(t, svc.SetMemberRepos(ctx, ownerP, org.ID, true))

	err = st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "space/stolen", AddedBy: member.ID, OwnerID: other.ID, AddedAt: now})
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "a repository is only ever in the adder's own space")

	require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, org.ID, accounts.DefaultRepoReadOnly))
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "space/shared", AddedBy: owner.ID, Visibility: accounts.Public, AddedAt: now}))
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "space/mine", AddedBy: member.ID, OwnerID: member.ID, Visibility: accounts.Public, AddedAt: now}))
	repos, err := st.OrgRepositories(ctx, org.ID)
	require.NoError(t, err)
	owners := map[string]string{}
	for _, r := range repos {
		owners[r.Graph] = r.OwnerID
	}
	assert.Equal(t, owners["space/mine"], member.ID, "the owner is stored")
	assert.Equal(t, owners["space/shared"], "")

	role := func(u accounts.User, graph string) accounts.Role {
		a, err := svc.AccessOf(ctx, u.ID)
		require.NoError(t, err)
		for _, r := range a.Repos {
			if r.OrgID == org.ID && r.Graph == graph {
				return r.Role
			}
		}
		return ""
	}
	assert.Equal(t, role(other, "space/shared"), accounts.Viewer, "organisation repositories follow the default")
	assert.Equal(t, role(other, "space/mine"), accounts.Role(""), "a member's own repository does not")
	assert.Equal(t, role(owner, "space/mine"), accounts.Role(""), "nor does an organisation owner see it")
	assert.Equal(t, role(member, "space/mine"), accounts.Admin, "its owner administers it")
	_, err = svc.SetRepoGrant(ctx, memberP, org.ID, "space/mine", accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: other.ID}, accounts.Viewer)
	require.NoError(t, err)
	assert.Equal(t, role(other, "space/mine"), accounts.Viewer, "the owner can share it")

	err = svc.RecoverRepoAdmin(ctx, ownerP, org.ID, "space/mine", owner.ID)
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "an organisation owner cannot recover a member's own repository")
	assert.Equal(t, role(owner, "space/mine"), accounts.Role(""), "so it stays out of the owner's reach")
	require.NoError(t, svc.RecoverRepoAdmin(ctx, ownerP, org.ID, "space/shared", owner.ID), "organisation repositories can still be recovered")
	assert.Equal(t, role(owner, "space/shared"), accounts.Admin)

	// The organisation adding a member's repository takes the entry over.
	moved := accounts.Repository{OrgID: org.ID, Graph: "space/mine", AddedBy: owner.ID, Visibility: accounts.Public, AddedAt: now}
	assert.True(t, errors.Is(st.MoveRepositoryToOrg(ctx, other.ID, moved), accounts.ErrNotFound), "only from the member who holds it")
	assert.True(t, errors.Is(st.MoveRepositoryToOrg(ctx, member.ID, accounts.Repository{OrgID: org.ID, Graph: "space/mine", AddedBy: owner.ID, OwnerID: member.ID, Visibility: accounts.Public}), accounts.ErrInvalidInput), "into the organisation's space only")
	assert.True(t, errors.Is(st.MoveRepositoryToOrg(ctx, member.ID, accounts.Repository{OrgID: org.ID, Graph: "space/mine", Visibility: accounts.Public}), accounts.ErrInvalidInput), "someone adds it")
	rows, err = st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "repo.space_moved", Limit: 10})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "refused moves leave no event")
	require.NoError(t, st.MoveRepositoryToOrg(ctx, member.ID, moved))
	repos, err = st.OrgRepositories(ctx, org.ID)
	require.NoError(t, err)
	for _, r := range repos {
		if r.Graph == "space/mine" {
			assert.False(t, r.Personal(), "it is the organisation's now")
		}
	}
	assert.Len(t, repos, 2, "moved, not copied")
	assert.Equal(t, role(member, "space/mine"), accounts.Admin, "the member keeps administering it")
	assert.Equal(t, role(owner, "space/mine"), accounts.Admin, "the adder administers it, as for any organisation add")
	assert.Equal(t, role(other, "space/mine"), accounts.Viewer, "everyone else gets the organisation's default")
	rows, err = st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "repo.space_moved", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the move is audited")
	assert.Equal(t, rows[0].Target, "space/mine")
	assert.True(t, errors.Is(st.MoveRepositoryToOrg(ctx, member.ID, moved), accounts.ErrNotFound), "only once")
}
