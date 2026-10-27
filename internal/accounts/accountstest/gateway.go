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

// RunGateway covers the organisation opt-in for the agent gateway: off by
// default, changed only by an admin in a signed-in session, audited, and kept
// per organisation.
func RunGateway(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"gw-owner", "gw-member"} {
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: id, Name: id, Email: id + "@example.com", CreatedAt: now}))
	}
	for _, id := range []string{"gw-org-a", "gw-org-b"} {
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: id, Name: id, Slug: id, CreatedAt: now}, "gw-owner"))
	}
	require.NoError(t, st.SetMember(ctx, "gw-org-a", "gw-member", accounts.Member))
	for _, u := range []string{"gw-owner", "gw-member"} {
		require.NoError(t, st.CreateSession(ctx, accounts.Session{TokenHash: "h-" + u[3:], UserID: u, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}))
	}
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }
	owner := accounts.Principal{User: accounts.User{ID: "gw-owner"}, Via: "session", SessionHash: "h-owner"}
	member := accounts.Principal{User: accounts.User{ID: "gw-member"}, Via: "session", SessionHash: "h-member"}
	key := accounts.Principal{User: accounts.User{ID: "gw-owner"}, Via: "key", OrgID: "gw-org-a"}

	on, err := svc.GatewayEnabled(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.False(t, on, "off until an organisation opts in")

	assert.True(t, errors.Is(svc.SetGateway(ctx, member, "gw-org-a", true), accounts.ErrForbidden), "members cannot opt in")
	assert.True(t, errors.Is(svc.SetGateway(ctx, key, "gw-org-a", true), accounts.ErrForbidden), "API keys cannot opt in")
	on, err = svc.GatewayEnabled(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.False(t, on)

	require.NoError(t, svc.SetGateway(ctx, owner, "gw-org-a", true))
	on, err = svc.GatewaySetting(ctx, member, "gw-org-a")
	require.NoError(t, err)
	assert.True(t, on)
	on, err = svc.GatewayEnabled(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.False(t, on, "the choice is per organisation")
	_, err = svc.GatewaySetting(ctx, member, "gw-org-b")
	assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound), "not a member of org b")

	rows, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: "gw-org-a", Action: "org.gateway.set", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the change is audited")
	assert.Equal(t, rows[0].Detail, "enabled=true")

	require.NoError(t, svc.SetGateway(ctx, owner, "gw-org-a", false))
	on, err = svc.GatewayEnabled(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.False(t, on)
}
