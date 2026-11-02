package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestOpenFGARebuild(t *testing.T) {
	off, _ := accountsServer(t)
	root := newClient(t, off)
	require.Equal(t, root.do("POST", "/api/v1/auth/login", `{"email":"`+accountstest.RootEmail+`","password":"`+accountstest.SetupPassword+`"}`, nil), 200)
	assert.Equal(t, root.do("POST", "/api/v1/admin/openfga/rebuild", "", nil), 409, "nothing to rebuild without OpenFGA")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), false)
	// Nothing answers there: the sync loop keeps retrying in the
	// background, and the request only marks organisations.
	cfg := fga.Config{URL: "https://openfga.invalid", Token: "test-key"}
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, ServiceToken: "svc-token", OpenFGA: &cfg}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	bootstrapAccounts(t, acc)
	acc.SignupOpen = true

	root = newClient(t, ts)
	require.Equal(t, root.do("POST", "/api/v1/auth/login", `{"email":"`+accountstest.RootEmail+`","password":"`+accountstest.SetupPassword+`"}`, nil), 200)
	bob := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 202)

	assert.Equal(t, bob.do("POST", "/api/v1/admin/openfga/rebuild", "", nil), 403, "platform admins only")
	var out struct{ Organisations int }
	require.Equal(t, root.do("POST", "/api/v1/admin/openfga/rebuild", "", &out), 202)
	assert.Equal(t, out.Organisations, 2, "the setup organisation and bob's")
	require.Equal(t, root.do("POST", "/api/v1/admin/openfga/rebuild?org="+signed.Org.ID, "", &out), 202)
	assert.Equal(t, out.Organisations, 1)
	assert.Equal(t, root.do("POST", "/api/v1/admin/openfga/rebuild?org=nope", "", nil), 404)

	evs, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "openfga.rebuild"})
	require.NoError(t, err)
	require.Len(t, evs, 2, "every request is audited, the refused ones are not stored")
	for _, e := range evs {
		assert.Equal(t, e.ActorEmail, accountstest.RootEmail)
	}
}
