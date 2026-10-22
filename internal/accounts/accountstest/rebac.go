package accountstest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// RunReBAC exercises the authoritative resolver and mutation rules on both stores.
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
	owner, ownerP := user("policy-owner@example.com")
	reader, readerP := user("policy-reader@example.com")
	guest, _ := user("policy-guest@example.com")
	outsider, outsiderP := user("policy-outsider@example.com")
	require.NoError(t, st.SetPlatformAdmin(ctx, outsider.ID, true))
	outsiderP.User.PlatformAdmin = true
	org, err := svc.CreateOrg(ctx, ownerP, "Policy Acme", "")
	require.NoError(t, err)
	for _, m := range []struct {
		u accounts.User
		r accounts.Role
	}{{reader, accounts.Viewer}, {guest, accounts.Guest}} {
		_, err := svc.AddMember(ctx, ownerP, org.ID, m.u.Email, m.r)
		require.NoError(t, err)
	}
	graph := "policy/api"
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, AddedBy: owner.ID, Visibility: accounts.Private, AddedAt: now}))
	subject := func(u accounts.User) accounts.Tuple {
		return accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: u.ID}
	}
	role := func(u accounts.User) accounts.Role {
		a, err := svc.AccessOf(ctx, u.ID)
		require.NoError(t, err)
		for _, r := range a.Repos {
			if r.OrgID == org.ID && r.Graph == graph {
				return r.Role
			}
		}
		return ""
	}
	assert.Equal(t, role(owner), accounts.Admin)
	assert.Equal(t, role(reader), accounts.Role(""), "new organisations default to NONE")
	_, err = svc.ResolveRepository(ctx, outsiderP, org.ID, graph, accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	_, err = svc.AddMember(ctx, outsiderP, org.ID, outsider.Email, accounts.Owner)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "platform admin cannot enrol itself")
	require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, org.ID, accounts.DefaultRepoReadOnly))
	assert.Equal(t, role(reader), accounts.Viewer)
	assert.Equal(t, role(guest), accounts.Role(""))
	team, err := svc.CreateTeam(ctx, ownerP, org.ID, "Builders")
	require.NoError(t, err)
	_, err = svc.SetTeamMember(ctx, ownerP, team.ID, reader.Email, accounts.RelMember)
	require.NoError(t, err)
	_, err = svc.SetTeamMember(ctx, ownerP, team.ID, outsider.Email, accounts.RelMember)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
	teamSub := accounts.Tuple{SubjectType: accounts.TypeTeam, SubjectID: team.ID, SubjectRelation: accounts.RelMember}
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, teamSub, accounts.Admin)
	require.NoError(t, err)
	assert.Equal(t, role(reader), accounts.Admin)
	for _, tc := range []struct {
		name        string
		grant, want accounts.Role
	}{{"direct read overrides team admin", accounts.Viewer, accounts.Viewer}, {"direct deny overrides team and default", accounts.None, ""}, {"direct write", accounts.Member, accounts.Member}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetRepoGrant(ctx, ownerP, org.ID, graph, subject(reader), tc.grant)
			require.NoError(t, err)
			assert.Equal(t, role(reader), tc.want)
		})
	}
	require.NoError(t, svc.RemoveRepoGrant(ctx, ownerP, org.ID, graph, subject(reader)))
	assert.Equal(t, role(reader), accounts.Admin)
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, teamSub, accounts.None)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "NONE applies only to users")
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, subject(outsider), accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "external shares need explicit membership")
	// Store foreign keys reject new repository grants without tenant membership.
	err = st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(org.ID, graph), Relation: accounts.RepoViewer, SubjectType: accounts.TypeUser, SubjectID: outsider.ID, CreatedAt: now})
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	assert.Equal(t, role(outsider), accounts.Role(""))
	a, err := svc.AccessOf(ctx, reader.ID)
	require.NoError(t, err)
	assert.Len(t, a.Scope(map[string]bool{"foreign": true}), 0)
	require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, org.ID, accounts.DefaultRepoNone))
	assert.Equal(t, role(reader), accounts.Admin)
	// Removing team membership removes inherited access immediately.
	require.NoError(t, svc.RemoveTeamMember(ctx, ownerP, team.ID, reader.ID))
	assert.Equal(t, role(reader), accounts.Role(""))
	// Organisation ownership does not bypass an explicit repository deny.
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, subject(reader), accounts.Admin)
	require.NoError(t, err)
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, subject(owner), accounts.None)
	require.NoError(t, err)
	assert.Equal(t, role(owner), accounts.Role(""))
	_, err = svc.SetRepoGrant(ctx, ownerP, org.ID, graph, subject(guest), accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrForbidden))
	require.NoError(t, svc.RecoverRepoAdmin(ctx, ownerP, org.ID, graph, owner.ID))
	// Two concurrent removals cannot remove the last administrator.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, u := range []accounts.User{owner, reader} {
		wg.Go(func() { errs <- svc.RemoveRepoGrant(ctx, ownerP, org.ID, graph, subject(u)) })
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else {
			assert.True(t, errors.Is(err, accounts.ErrLastRepoAdmin) || errors.Is(err, accounts.ErrForbidden))
		}
	}
	assert.Equal(t, successes, 1)
	require.NoError(t, svc.RecoverRepoAdmin(ctx, ownerP, org.ID, graph, owner.ID))
	// Suspension overrides even the last repository administrator; recovery remains available.
	disabled := now
	require.NoError(t, st.SetUserDisabled(ctx, reader.ID, &disabled))
	_, err = svc.AccessOf(ctx, reader.ID)
	assert.True(t, errors.Is(err, accounts.ErrSuspended))
	_, err = svc.ResolveRepository(ctx, readerP, org.ID, graph, accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
	require.NoError(t, st.SetUserDisabled(ctx, reader.ID, nil))
	token, key, err := svc.CreateKey(ctx, ownerP, org.ID, "policy test", 0)
	require.NoError(t, err)
	kp, err := svc.AuthenticateKey(ctx, token)
	require.NoError(t, err)
	_, err = svc.ResolveRepository(ctx, kp, org.ID, graph, accounts.Admin)
	require.NoError(t, err)
	require.NoError(t, svc.RevokeKey(ctx, ownerP, key.ID))
	_, err = svc.ResolveRepository(ctx, kp, org.ID, graph, accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrKeyRevoked))
	require.NoError(t, st.SetRepositoryAccess(ctx, org.ID, graph, accounts.AccessLost, now))
	assert.Equal(t, role(owner), accounts.Role(""))
	require.NoError(t, st.SetRepositoryAccess(ctx, org.ID, graph, accounts.AccessOK, now))
	// Transaction failure rolls policy and outbox back.
	before, err := st.OrgByID(ctx, org.ID)
	require.NoError(t, err)
	rollback := errors.New("rollback")
	err = st.PolicyTransaction(ctx, org.ID, func(tx accounts.Store) error {
		require.NoError(t, tx.SetDefaultRepoAccess(ctx, org.ID, accounts.DefaultRepoReadOnly))
		require.NoError(t, tx.TouchPolicy(ctx, org.ID))
		return rollback
	})
	assert.True(t, errors.Is(err, rollback))
	after, err := st.OrgByID(ctx, org.ID)
	require.NoError(t, err)
	assert.Equal(t, after, before)
	pending, err := st.PendingAccessPolicies(ctx, accounts.MaxTuples)
	require.NoError(t, err)
	found := false
	for _, o := range pending {
		if o.ID == org.ID {
			found = true
		}
	}
	assert.True(t, found)
	require.NoError(t, st.AcknowledgeAccessPolicy(ctx, org.ID, before.PolicyRevision-1))
	pending, err = st.PendingAccessPolicies(ctx, accounts.MaxTuples)
	require.NoError(t, err)
	found = false
	for _, o := range pending {
		if o.ID == org.ID {
			found = true
		}
	}
	assert.True(t, found, "stale acknowledgement preserves newer work")
	// The initial grant, policy updates and explicit recovery have audit records.
	evs, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Limit: 200})
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, e := range evs {
		actions[e.Action] = true
	}
	for _, action := range []string{"repo.admin.initial", "org.repo_policy.set", "repo.admin.recover", "repo.grant.set"} {
		assert.True(t, actions[action], action)
	}
}

// RunPolicyRecovery covers malformed legacy grants without restoring broad access.
func RunPolicyRecovery(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc := accounts.NewService(st, true)
	users := []accounts.User{
		{ID: "recovery-owner", Email: "recovery-owner@example.com", CreatedAt: now},
		{ID: "recovery-admin", Email: "recovery-admin@example.com", CreatedAt: now},
		{ID: "recovery-member", Email: "recovery-member@example.com", CreatedAt: now},
	}
	for _, u := range users {
		require.NoError(t, st.CreateUser(ctx, u))
	}
	orgID, graph := "recovery-org", "recovery/repo"
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: orgID, Slug: orgID, Name: "Recovery", CreatedAt: now}, users[0].ID))
	for _, u := range users[1:] {
		require.NoError(t, st.SetMember(ctx, orgID, u.ID, accounts.Member))
	}
	ownerP := accounts.Principal{User: users[0], Via: "session"}
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: orgID, Graph: graph, AddedBy: users[0].ID, Visibility: accounts.Public, AddedAt: now}))
	require.NoError(t, svc.SetDefaultRepoAccess(ctx, ownerP, orgID, accounts.DefaultRepoReadOnly))
	team := accounts.Team{ID: "recovery-team", OrgID: orgID, Slug: "recovery-team", Name: "Recovery", CreatedAt: now}
	require.NoError(t, st.CreateTeam(ctx, team))
	for _, u := range users {
		require.NoError(t, st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeTeam, ObjectID: team.ID, Relation: accounts.RelMember, SubjectType: accounts.TypeUser, SubjectID: u.ID, CreatedAt: now}))
	}
	require.NoError(t, st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(orgID, graph), Relation: "invalid", SubjectType: accounts.TypeTeam, SubjectID: team.ID, SubjectRelation: accounts.RelMember, CreatedAt: now}))
	_, err := svc.AccessOf(ctx, users[0].ID)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
	report, err := svc.AccessChangeReport(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, report.MalformedGrants, 1)
	assert.Equal(t, report.OrphanedRepositories, []string{graph})
	require.NoError(t, svc.Require(ctx, ownerP, orgID, accounts.Owner))
	require.NoError(t, svc.RecoverRepoAdmin(ctx, ownerP, orgID, graph, users[1].ID))
	for i, u := range users {
		a, err := svc.AccessOf(ctx, u.ID)
		require.NoError(t, err)
		if i == 1 {
			require.Len(t, a.Repos, 1)
			assert.Equal(t, a.Repos[0].Role, accounts.Admin)
		} else {
			assert.Len(t, a.Repos, 0, "repair preserves denial for other affected members, including the owner")
		}
	}
	report, err = svc.AccessChangeReport(ctx, orgID)
	require.NoError(t, err)
	assert.Equal(t, report.MalformedGrants, 0)
	assert.Len(t, report.OrphanedRepositories, 0)
	events, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: orgID, Action: "repo.admin.recover"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, events[0].Detail, "repaired 1 malformed grants")
}
