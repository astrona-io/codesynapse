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

// RunGateway covers the organisation setting for the agent gateway: on by
// default, switched off only by an admin in a signed-in session, audited, and
// kept per organisation.
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
	assert.True(t, on, "on until an organisation switches it off")

	assert.True(t, errors.Is(svc.SetGateway(ctx, member, "gw-org-a", false), accounts.ErrForbidden), "members cannot switch it off")
	assert.True(t, errors.Is(svc.SetGateway(ctx, key, "gw-org-a", false), accounts.ErrForbidden), "API keys cannot switch it off")
	on, err = svc.GatewayEnabled(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.True(t, on)

	require.NoError(t, svc.SetGateway(ctx, owner, "gw-org-a", false))
	set, err := svc.GatewaySetting(ctx, member, "gw-org-a")
	require.NoError(t, err)
	assert.False(t, set.Enabled)
	assert.True(t, set.LocalFirst)
	on, err = svc.GatewayEnabled(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.True(t, on, "the choice is per organisation")
	_, err = svc.GatewaySetting(ctx, member, "gw-org-b")
	assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound), "not a member of org b")

	rows, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: "gw-org-a", Action: "org.gateway.set", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the change is audited")
	assert.Equal(t, rows[0].Detail, "enabled=false")

	require.NoError(t, svc.SetGateway(ctx, owner, "gw-org-a", true))
	on, err = svc.GatewayEnabled(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.True(t, on)

	// Local first: on by default, also for a row written only for enabled.
	first, err := svc.GatewayLocalFirst(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.True(t, first, "a row written for enabled keeps local first")
	first, err = svc.GatewayLocalFirst(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.True(t, first, "local first until an organisation switches it off")
	assert.True(t, errors.Is(svc.SetGatewayLocalFirst(ctx, member, "gw-org-b", false), accounts.ErrForbidden), "members cannot change it")
	assert.True(t, errors.Is(svc.SetGatewayLocalFirst(ctx, key, "gw-org-a", false), accounts.ErrForbidden), "API keys cannot change it")

	require.NoError(t, svc.SetGatewayLocalFirst(ctx, owner, "gw-org-b", false))
	first, err = svc.GatewayLocalFirst(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.False(t, first)
	on, err = svc.GatewayEnabled(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.True(t, on, "turning local first off leaves the gateway on")
	set, err = svc.GatewaySetting(ctx, owner, "gw-org-b")
	require.NoError(t, err)
	assert.Equal(t, set, accounts.GatewaySettings{Enabled: true, LocalFirst: false})
	first, err = svc.GatewayLocalFirst(ctx, "gw-org-a")
	require.NoError(t, err)
	assert.True(t, first, "the choice is per organisation")

	rows, err = st.ListEvents(ctx, accounts.EventFilter{OrgID: "gw-org-b", Action: "org.gateway.local_first.set", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the change is audited")
	assert.Equal(t, rows[0].Detail, "local_first=false")

	require.NoError(t, svc.SetGatewayLocalFirst(ctx, owner, "gw-org-b", true))
	first, err = svc.GatewayLocalFirst(ctx, "gw-org-b")
	require.NoError(t, err)
	assert.True(t, first)
	err = svc.SetGatewayLocalFirst(ctx, owner, "gw-org-missing", false)
	assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound), "unknown organisation")
}
