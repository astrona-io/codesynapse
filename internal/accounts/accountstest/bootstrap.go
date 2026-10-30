package accountstest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunBootstrap checks bootstrap-root separation (docs/ROOT_BOOTSTRAP_PLAN.md).
// newStore returns an empty, migrated store for each scenario.
func RunBootstrap(t *testing.T, newStore func(t *testing.T) accounts.Store) {
	t.Run("fresh setup", func(t *testing.T) { runFreshSetup(t, newStore(t)) })
	t.Run("concurrent handover", func(t *testing.T) { runConcurrentHandover(t, newStore(t)) })
	t.Run("root is denied business work", func(t *testing.T) { runRootDenied(t, newStore(t)) })
	t.Run("migration with history", func(t *testing.T) { runMigration(t, newStore(t)) })
	t.Run("ambiguous migration", func(t *testing.T) { runAmbiguousMigration(t, newStore(t)) })
	t.Run("migration needing handover", func(t *testing.T) { runPendingMigration(t, newStore(t)) })
}

func runFreshSetup(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc := accounts.NewService(st, true)
	needs, err := svc.Status(ctx)
	require.NoError(t, err)
	assert.True(t, needs)

	root, org, err := svc.SignUp(ctx, RootEmail, "Root", SetupPassword)
	require.NoError(t, err)
	assert.True(t, root.BootstrapRoot)
	assert.True(t, root.PlatformAdmin, "root administers the platform")
	assert.Equal(t, org.ID, "", "root gets no organisation")
	setup, err := svc.SetupState(ctx)
	require.NoError(t, err)
	assert.Equal(t, setup, accounts.Setup{State: accounts.SetupPending, RootUserID: root.ID})
	_, _, err = svc.SignUp(ctx, "early@example.com", "Early", pw)
	assert.True(t, errors.Is(err, accounts.ErrSetupRequired), "no other accounts before the handover")

	p, err := login(ctx, svc, RootEmail)
	require.NoError(t, err)
	_, err = svc.CreateOrg(ctx, p, "Root's own", "")
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))

	for name, in := range map[string]accounts.SetupInput{
		"weak password":        {OrgName: "Acme", Email: "owner@example.com", Name: "Owner", Password: "short"},
		"no organisation":      {Email: "owner@example.com", Name: "Owner", Password: pw},
		"both organisations":   {OrgName: "Acme", OrgID: "x", Email: "owner@example.com", Name: "Owner", Password: pw},
		"root's own email":     {OrgName: "Acme", Email: RootEmail, Name: "Owner", Password: pw},
		"unknown organisation": {OrgID: "missing", Email: "owner@example.com", Name: "Owner", Password: pw},
	} {
		_, err := svc.CompleteSetup(ctx, p, in)
		assert.Error(t, err, name)
	}
	n, err := st.CountUsers(ctx)
	require.NoError(t, err)
	assert.Equal(t, n, 1, "failed handovers leave nothing behind")
	orgs, err := st.ListOrgs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, orgs, 0)

	_, _, err = svc.SignUp(ctx, "x@example.com", "X", pw)
	assert.True(t, errors.Is(err, accounts.ErrSetupRequired))
	_, err = svc.CompleteSetup(ctx, accounts.Principal{User: root, Via: "key"}, accounts.SetupInput{OrgName: "Acme", Email: "owner@example.com", Name: "Owner", Password: pw})
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "only a signed-in session")

	done, err := svc.CompleteSetup(ctx, p, accounts.SetupInput{OrgName: "Acme", Email: "Owner@Example.com", Name: "Owner", Password: pw})
	require.NoError(t, err)
	assert.Equal(t, done.State, accounts.SetupComplete)
	_, err = st.SessionByHash(ctx, p.SessionHash)
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "the setup session ended")
	ownerUser, err := st.UserByID(ctx, done.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, ownerUser.Email, "owner@example.com")
	assert.False(t, ownerUser.BootstrapRoot || ownerUser.PlatformAdmin, "the owner is a standard account")
	role, err := st.MemberRole(ctx, done.OrgID, done.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Owner)
	_, err = st.MemberRole(ctx, done.OrgID, root.ID)
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "root does not join the organisation")

	p, err = login(ctx, svc, RootEmail)
	require.NoError(t, err)
	_, err = svc.CompleteSetup(ctx, p, accounts.SetupInput{OrgName: "Again", Email: "again@example.com", Name: "Again", Password: pw})
	assert.True(t, errors.Is(err, accounts.ErrSetupComplete), "retries create nothing")
	assert.Error(t, st.SaveSetup(ctx, accounts.Setup{State: accounts.SetupPending, RootUserID: root.ID}), "completion is permanent")
	needs, err = svc.Status(ctx)
	require.NoError(t, err)
	assert.False(t, needs)
	_, _, err = svc.SignUp(ctx, "later@example.com", "Later", pw)
	require.NoError(t, err, "standard sign-ups work after the handover")
}

func runConcurrentHandover(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc := accounts.NewService(st, true)
	var wg sync.WaitGroup
	roots := make([]error, 6)
	for i := range roots {
		wg.Go(func() {
			_, _, roots[i] = svc.SignUp(ctx, fmt.Sprintf("root%d@example.com", i), "Root", SetupPassword)
		})
	}
	wg.Wait()
	all, err := st.ListUsers(ctx, 10)
	require.NoError(t, err)
	created := 0
	for _, u := range all {
		if u.BootstrapRoot {
			created++
		}
	}
	assert.Equal(t, created, 1, "concurrent first sign-ups create one root")
	var root accounts.User
	for _, u := range all {
		if u.BootstrapRoot {
			root = u
		}
	}
	p, err := login(ctx, svc, root.Email)
	require.NoError(t, err)
	results := make([]error, 8)
	for i := range results {
		wg.Go(func() {
			_, results[i] = svc.CompleteSetup(ctx, p, accounts.SetupInput{OrgName: "Acme", Email: fmt.Sprintf("owner%d@example.com", i), Name: "Owner", Password: pw})
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, accounts.ErrSetupComplete) && !errors.Is(err, accounts.ErrUnauthorized):
			t.Errorf("unexpected handover error: %v", err)
		}
	}
	assert.Equal(t, ok, 1, "one handover commits")
	orgs, err := st.ListOrgs(ctx, 10)
	require.NoError(t, err)
	assert.Len(t, orgs, 1)
}

func runRootDenied(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc := accounts.NewService(st, true)
	rootP, err := Bootstrap(ctx, svc)
	require.NoError(t, err)
	setup, err := svc.SetupState(ctx)
	require.NoError(t, err)
	org := setup.OrgID

	// Memberships and grants added behind the service's back change nothing.
	require.NoError(t, st.SetMember(ctx, org, rootP.User.ID, accounts.Owner))
	_, err = svc.AccessOf(ctx, rootP.User.ID)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.RoleIn(ctx, rootP, org)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	assert.Error(t, svc.Require(ctx, rootP, org, accounts.Viewer))
	_, _, err = svc.CreateKey(ctx, rootP, org, "root key", time.Hour)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.ResolveRepository(ctx, rootP, org, "acme/api", accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.RegisterExecutionSession(ctx, rootP.User.ID, org, "root-session-1")
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	restricted, err := svc.Restricted(ctx, rootP.User.ID)
	require.NoError(t, err)
	assert.True(t, restricted)

	// Governance still works, and the platform flag never changes identity.
	_, _, err = svc.SignUp(ctx, "dana@example.com", "Dana", pw)
	require.NoError(t, err)
	dana, err := st.UserByEmail(ctx, "dana@example.com")
	require.NoError(t, err)
	require.NoError(t, svc.SetPlatformAdmin(ctx, rootP, dana.ID, true))
	restricted, err = svc.Restricted(ctx, dana.ID)
	require.NoError(t, err)
	assert.False(t, restricted, "a standard platform admin keeps business work")
	danaP, err := loginWith(ctx, svc, "dana@example.com", pw)
	require.NoError(t, err)
	require.NoError(t, svc.SetPlatformAdmin(ctx, danaP, rootP.User.ID, false))
	root, err := st.UserByID(ctx, rootP.User.ID)
	require.NoError(t, err)
	assert.True(t, root.BootstrapRoot && !root.PlatformAdmin, "revoking platform administration keeps root root")
	_, err = svc.AccessOf(ctx, root.ID)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	assert.True(t, errors.Is(st.MarkBootstrapRoot(ctx, dana.ID), accounts.ErrExists), "one root")

	// Owner recovery promotes a distinct standard member; root's ownership
	// does not count as an owner who can act.
	require.NoError(t, svc.SetPlatformAdmin(ctx, danaP, rootP.User.ID, true))
	rootP, err = login(ctx, svc, RootEmail)
	require.NoError(t, err)
	require.NoError(t, st.SetMember(ctx, org, dana.ID, accounts.Member))
	assert.True(t, errors.Is(svc.RecoverOrgOwner(ctx, rootP, org, rootP.User.ID), accounts.ErrForbidden))
	require.NoError(t, svc.RecoverOrgOwner(ctx, rootP, org, dana.ID))
	role, err := st.MemberRole(ctx, org, dana.ID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Owner)
	setupOwner, err := st.UserByEmail(ctx, SetupOwner)
	require.NoError(t, err)
	_, err = svc.SetMemberRole(ctx, danaP, org, setupOwner.ID, accounts.Member)
	require.NoError(t, err, "dana remains an owner who can act")
	_, err = svc.SetMemberRole(ctx, danaP, org, dana.ID, accounts.Member)
	assert.True(t, errors.Is(err, accounts.ErrLastOwner), "root's ownership does not count")
}

// legacy builds an installation that predates bootstrap separation: ada
// signed up first and became platform admin, bob owns another organisation.
func legacy(t *testing.T, st accounts.Store, withHistory, withStandardOwner bool) (*accounts.Service, accounts.User, accounts.Org) {
	t.Helper()
	ctx := context.Background()
	svc := accounts.NewService(st, true)
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	hash, err := accounts.HashPassword(pw)
	require.NoError(t, err)
	ada := accounts.User{ID: "u-ada", Email: "ada@example.com", Name: "Ada", PlatformAdmin: true, CreatedAt: now, PasswordHash: hash}
	require.NoError(t, st.CreateUser(ctx, ada))
	adaOrg := accounts.Org{ID: "o-ada", Slug: "ada", Name: "Ada's", CreatedAt: now}
	require.NoError(t, st.CreateOrg(ctx, adaOrg, ada.ID))
	if withHistory {
		_, err := svc.Record(ctx, accounts.Event{UserID: ada.ID, Via: "anonymous", Action: "user.signup", Target: ada.Email})
		require.NoError(t, err)
	}
	if withStandardOwner {
		bob := accounts.User{ID: "u-bob", Email: "bob@example.com", Name: "Bob", CreatedAt: now.Add(time.Hour), PasswordHash: hash}
		require.NoError(t, st.CreateUser(ctx, bob))
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o-bob", Slug: "bob", Name: "Bob's", CreatedAt: now}, bob.ID))
	}
	return svc, ada, adaOrg
}

func runMigration(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc, ada, adaOrg := legacy(t, st, true, true)
	adaP, err := loginWith(ctx, svc, ada.Email, pw)
	require.NoError(t, err)
	secret, _, err := svc.CreateKey(ctx, adaP, adaOrg.ID, "pre-migration key", time.Hour)
	require.NoError(t, err, "before migration ada works as before")
	_, err = svc.RegisterExecutionSession(ctx, ada.ID, adaOrg.ID, "queued-before-migration")
	require.NoError(t, err)
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: adaOrg.ID, Graph: "acme/api", Visibility: accounts.Public, AddedBy: ada.ID, AddedAt: time.Now()}))

	setup, err := svc.ReconcileBootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, setup.State, accounts.SetupComplete)
	assert.Equal(t, setup.RootUserID, ada.ID)
	assert.Equal(t, setup.OwnerID, "u-bob")
	again, err := svc.ReconcileBootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, again.RootUserID, setup.RootUserID, "reconciliation runs once")
	_, err = svc.AuthenticateKey(ctx, secret)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount), "root's old keys stop working")
	// Work queued before the migration rechecks before it runs or writes.
	_, err = svc.ResolveExecutionSession(ctx, ada.ID, adaOrg.ID, "queued-before-migration")
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.ResolveRepository(ctx, adaP, adaOrg.ID, "acme/api", accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.ResolveRepository(ctx, accounts.Principal{User: ada, OrgID: adaOrg.ID, Via: "automation"}, adaOrg.ID, "acme/api", accounts.Admin)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount), "schedules act as their delegating account")
	_, err = svc.AccessOf(ctx, ada.ID)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	_, err = svc.AccessOf(ctx, "u-bob")
	require.NoError(t, err, "standard users are unaffected")
	events, err := st.ListEvents(ctx, accounts.EventFilter{Action: "setup.migrate"})
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

func runAmbiguousMigration(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc, ada, _ := legacy(t, st, false, true)
	setup, err := svc.ReconcileBootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, setup.State, accounts.SetupAmbiguous, "no setup history: never guess")
	_, err = svc.AccessOf(ctx, ada.ID)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount), "platform admins fail closed")
	_, err = svc.AccessOf(ctx, "u-bob")
	require.NoError(t, err)
	_, err = svc.DesignateRoot(ctx, "bob@example.com")
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "the root must be a platform admin")
	setup, err = svc.DesignateRoot(ctx, ada.Email)
	require.NoError(t, err)
	assert.Equal(t, setup.State, accounts.SetupComplete)
	assert.Equal(t, setup.RootUserID, ada.ID)
	_, err = svc.DesignateRoot(ctx, ada.Email)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "designation happens once")
	events, err := st.ListEvents(ctx, accounts.EventFilter{Action: "setup.designate_root"})
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

func runPendingMigration(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc, ada, adaOrg := legacy(t, st, true, false)
	setup, err := svc.ReconcileBootstrap(ctx)
	require.NoError(t, err)
	assert.Equal(t, setup.State, accounts.SetupPending, "no standard owner yet")
	p, err := loginWith(ctx, svc, ada.Email, pw)
	require.NoError(t, err)
	done, err := svc.CompleteSetup(ctx, p, accounts.SetupInput{OrgID: adaOrg.ID, Email: "heir@example.com", Name: "Heir", Password: pw})
	require.NoError(t, err)
	assert.Equal(t, done.OrgID, adaOrg.ID, "the existing organisation is handed over")
	role, err := st.MemberRole(ctx, adaOrg.ID, done.OwnerID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Owner)
}
