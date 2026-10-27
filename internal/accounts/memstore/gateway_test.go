package memstore

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestGatewayExecutionSession(t *testing.T) {
	ctx := context.Background()
	st := New()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "gw-owner", Email: "gw@example.com", CreatedAt: now}))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "gw-org", Name: "gw", Slug: "gw", CreatedAt: now}, "gw-owner"))
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }
	key := accounts.Principal{User: accounts.User{ID: "gw-owner"}, Via: "key", KeyID: "key-1", OrgID: "gw-org"}

	id, err := svc.GatewayExecutionSession(ctx, key, "gw-org")
	require.NoError(t, err)
	assert.Equal(t, id.ParentSessionID, id.SessionID)
	again, err := svc.GatewayExecutionSession(ctx, key, "gw-org")
	require.NoError(t, err)
	assert.Equal(t, again, id)

	other := key
	other.KeyID = "key-2"
	second, err := svc.GatewayExecutionSession(ctx, other, "gw-org")
	require.NoError(t, err)
	assert.NotEqual(t, second.SessionID, id.SessionID)

	for name, p := range map[string]accounts.Principal{
		"browser session": {User: key.User, Via: "session", SessionHash: "h", OrgID: "gw-org"},
		"no key id":       {User: key.User, Via: "key", OrgID: "gw-org"},
		"other org":       {User: key.User, Via: "key", KeyID: "key-1", OrgID: "elsewhere"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.GatewayExecutionSession(ctx, p, "gw-org")
			require.Error(t, err)
		})
	}
}
