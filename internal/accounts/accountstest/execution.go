package accountstest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// RunExecutionSessions covers binding, replay rejection and concurrent admission.
func RunExecutionSessions(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"exec-owner", "exec-peer", "exec-outsider"} {
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: id, Name: id, Email: id + "@example.com", CreatedAt: now, PlatformAdmin: id == "exec-outsider"}))
	}
	for _, id := range []string{"exec-org-a", "exec-org-b", "exec-cap-org"} {
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: id, Name: id, Slug: id, CreatedAt: now}, "exec-owner"))
	}
	require.NoError(t, st.SetMember(ctx, "exec-org-a", "exec-peer", accounts.Member))
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }
	id, err := svc.RegisterExecutionSession(ctx, "exec-owner", "exec-org-a", "exec-chat-a")
	require.NoError(t, err)
	assert.Equal(t, id.ParentSessionID, id.SessionID)
	first, err := st.ExecutionSessionByID(ctx, id.SessionID)
	require.NoError(t, err)
	assert.Equal(t, first.ExpiresAt, now.Add(accounts.ExecutionSessionTTL))
	now = now.Add(time.Hour)
	again, err := svc.RegisterExecutionSession(ctx, "exec-owner", "exec-org-a", id.SessionID)
	require.NoError(t, err)
	assert.Equal(t, again, id)
	second, err := st.ExecutionSessionByID(ctx, id.SessionID)
	require.NoError(t, err)
	assert.Equal(t, first, second, "registration cannot extend expiry")
	for _, tc := range []struct{ user, org string }{{"exec-peer", "exec-org-a"}, {"exec-owner", "exec-org-b"}, {"exec-outsider", "exec-org-a"}} {
		_, err := svc.RegisterExecutionSession(ctx, tc.user, tc.org, id.SessionID)
		require.Error(t, err)
		_, err = svc.ResolveExecutionSession(ctx, tc.user, tc.org, id.SessionID)
		require.Error(t, err)
		require.Error(t, svc.RevokeExecutionSession(ctx, tc.user, tc.org, id.SessionID))
	}
	_, err = svc.RegisterExecutionSession(ctx, "exec-outsider", "exec-org-a", "exec-admin-bypass")
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "platform privileges grant no session membership")
	_, err = svc.ResolveExecutionSession(ctx, "exec-owner", "exec-org-a", "never-registered")
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed))
	bound, err := execution.With(ctx, id)
	require.NoError(t, err)
	_, err = svc.RegisterExecutionSession(bound, "exec-owner", "exec-org-a", "replacement-chat")
	assert.True(t, errors.Is(err, execution.ErrConflict))
	require.NoError(t, svc.RevokeExecutionSession(ctx, "exec-owner", "exec-org-a", id.SessionID))
	require.NoError(t, svc.RevokeExecutionSession(ctx, "exec-owner", "exec-org-a", id.SessionID), "cleanup retry is idempotent")
	_, err = svc.ResolveExecutionSession(ctx, "exec-owner", "exec-org-a", id.SessionID)
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed))
	_, err = svc.RegisterExecutionSession(ctx, "exec-owner", "exec-org-a", id.SessionID)
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed), "revoked IDs never restart")

	_, err = svc.RegisterExecutionSession(ctx, "exec-peer", "exec-org-a", "exec-peer-chat")
	require.NoError(t, err)
	require.NoError(t, st.RemoveMember(ctx, "exec-org-a", "exec-peer"))
	_, err = svc.ResolveExecutionSession(ctx, "exec-peer", "exec-org-a", "exec-peer-chat")
	assert.True(t, errors.Is(err, accounts.ErrForbidden))
	require.NoError(t, st.SetMember(ctx, "exec-org-a", "exec-peer", accounts.Member))
	require.NoError(t, st.SetUserDisabled(ctx, "exec-peer", &now))
	_, err = svc.ResolveExecutionSession(ctx, "exec-peer", "exec-org-a", "exec-peer-chat")
	assert.True(t, errors.Is(err, accounts.ErrForbidden))
	require.NoError(t, st.SetUserDisabled(ctx, "exec-peer", nil))

	_, err = svc.RegisterExecutionSession(ctx, "exec-owner", "exec-org-a", "exec-expiring")
	require.NoError(t, err)
	now = now.Add(accounts.ExecutionSessionTTL)
	_, err = svc.ResolveExecutionSession(ctx, "exec-owner", "exec-org-a", "exec-expiring")
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed))
	now = now.Add(-accounts.ExecutionSessionTTL)
	_, err = svc.RegisterExecutionSession(ctx, "exec-owner", "exec-org-a", "exec-expiring")
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed), "observed expiry is irreversible even after clock rollback")
	closed, err := st.ClosedExecutionSessions(ctx, now, "", 1)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	next, err := st.ClosedExecutionSessions(ctx, now, closed[0].ID, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.True(t, next[0].ID > closed[0].ID)

	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			user := "exec-owner"
			if i%2 == 0 {
				user = "exec-peer"
			}
			if _, err := svc.RegisterExecutionSession(ctx, user, "exec-org-a", "exec-race-binding"); err == nil {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, admitted.Load(), int32(8), "only one participant wins the shared ID")

	for i := range accounts.MaxActiveExecutionSessions - 1 {
		_, _, err := st.RegisterExecutionSession(ctx, accounts.ExecutionSession{ID: fmt.Sprintf("exec-cap-%04d", i), OrgID: "exec-cap-org", UserID: "exec-owner", CreatedAt: now, ExpiresAt: now.Add(accounts.ExecutionSessionTTL)})
		require.NoError(t, err)
	}
	admitted.Store(0)
	for i := range 8 {
		wg.Go(func() {
			_, err := svc.RegisterExecutionSession(ctx, "exec-owner", "exec-cap-org", fmt.Sprintf("exec-cap-race-%d", i))
			if err == nil {
				admitted.Add(1)
			} else {
				assert.True(t, errors.Is(err, accounts.ErrExecutionSessionLimit))
			}
		})
	}
	wg.Wait()
	assert.Equal(t, admitted.Load(), int32(1), "active cap is atomic across concurrent callers")
	verification, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.True(t, verification.OK)
}
