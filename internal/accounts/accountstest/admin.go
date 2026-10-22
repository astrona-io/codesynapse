package accountstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// RunAdmin exercises platform administration. It creates its own users and
// may revoke other platform admins (the store is a test store).
func RunAdmin(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	require.NoError(t, st.Migrate(ctx))
	svc := accounts.NewService(st, true)
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	admin, _, err := svc.SignUp(ctx, "admin-root@example.com", "Root", pw)
	require.NoError(t, err)
	require.NoError(t, st.SetPlatformAdmin(ctx, admin.ID, true))
	admin.PlatformAdmin = true
	adminP := accounts.Principal{User: admin, Via: "session"}
	user, org, err := svc.SignUp(ctx, "admin-user@example.com", "User", pw)
	require.NoError(t, err)
	userP := accounts.Principal{User: user, Via: "session"}
	key, _, err := svc.CreateKey(ctx, userP, org.ID, "laptop", 0)
	require.NoError(t, err)
	tok, _, err := svc.Login(ctx, user.Email, pw, "10.0.0.1")
	require.NoError(t, err)

	assert.True(t, errors.Is(svc.SuspendUser(ctx, userP, admin.ID), accounts.ErrForbidden), "only platform admins")
	assert.True(t, errors.Is(svc.SuspendUser(ctx, accounts.Principal{User: admin, Via: "key"}, user.ID), accounts.ErrForbidden), "never through a key")
	assert.True(t, errors.Is(svc.SuspendUser(ctx, adminP, admin.ID), accounts.ErrInvalidInput), "not yourself")

	// A suspension stops sign-in, the open session and the API key at once.
	require.NoError(t, svc.SuspendUser(ctx, adminP, user.ID))
	_, _, err = svc.Login(ctx, user.Email, pw, "10.0.0.1")
	assert.True(t, errors.Is(err, accounts.ErrSuspended))
	_, err = svc.Authenticate(ctx, tok)
	assert.Error(t, err, "sessions end")
	_, err = svc.AuthenticateKey(ctx, key)
	assert.Error(t, err, "keys stop working")
	u, err := st.UserByID(ctx, user.ID)
	require.NoError(t, err)
	require.NotNil(t, u.DisabledAt)
	assert.True(t, u.DisabledAt.Equal(now))

	// Resuming restores the key; the ended session stays ended.
	require.NoError(t, svc.ResumeUser(ctx, adminP, user.ID))
	_, err = svc.AuthenticateKey(ctx, key)
	assert.NoError(t, err)
	_, _, err = svc.Login(ctx, user.Email, pw, "10.0.0.2")
	assert.NoError(t, err)

	// Platform admins: granted, revoked, never the last active one.
	require.NoError(t, svc.SetPlatformAdmin(ctx, adminP, user.ID, true))
	users, err := st.ListUsers(ctx, 100000)
	require.NoError(t, err)
	for _, x := range users {
		if x.PlatformAdmin && x.ID != admin.ID && x.ID != user.ID {
			require.NoError(t, st.SetPlatformAdmin(ctx, x.ID, false)) // leave exactly these two
		}
	}
	require.NoError(t, svc.SetPlatformAdmin(ctx, adminP, user.ID, false))
	assert.True(t, errors.Is(svc.SetPlatformAdmin(ctx, adminP, admin.ID, false), accounts.ErrLastAdmin), "someone must remain")

	// The audit trail answers "what did the admin do" and "which logins failed".
	evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: "user.", Actor: admin.Email, Limit: 100})
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, e := range evs {
		actions[e.Action] = true
		assert.Equal(t, e.ActorEmail, admin.Email)
	}
	for _, a := range []string{"user.suspend", "user.resume", "user.platform_admin.grant", "user.platform_admin.revoke"} {
		assert.True(t, actions[a], "audited: "+a)
	}
	failed, err := st.ListEvents(ctx, accounts.EventFilter{Action: "user.login", Outcome: accounts.Failure, Limit: 100})
	require.NoError(t, err)
	suspended := false
	for _, e := range failed {
		assert.Equal(t, e.Outcome, accounts.Failure)
		suspended = suspended || e.Detail == "account suspended"
	}
	assert.True(t, suspended)
}
