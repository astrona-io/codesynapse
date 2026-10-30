package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestInvitationsOverHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.signupFloor = 200 * time.Millisecond
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	bob, carol, dave := signup("bob@example.com"), signup("carol@example.com"), signup("dave@example.com")
	var acme accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	base := "/api/v1/accounts/orgs/" + acme.Slug + "/invitations"

	type created struct {
		status  int
		body    map[string]json.RawMessage
		token   string
		id      string
		elapsed time.Duration
	}
	create := func(c *client, email, role string) created {
		t.Helper()
		var out created
		start := time.Now()
		var raw string
		out.status = c.do("POST", base, `{"email":"`+email+`","role":"`+role+`"}`, &raw)
		out.elapsed = time.Since(start)
		if out.status == http.StatusCreated {
			require.NoError(t, json.Unmarshal([]byte(raw), &out.body))
			require.NoError(t, json.Unmarshal(out.body["token"], &out.token))
			var inv accounts.Invitation
			require.NoError(t, json.Unmarshal(out.body["invitation"], &inv))
			out.id = inv.ID
		}
		return out
	}
	keys := func(m map[string]json.RawMessage) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}

	t.Run("registered and unknown emails answer alike", func(t *testing.T) {
		registered := create(bob, "carol@example.com", "member")
		unknown := create(bob, "nobody@example.com", "member")
		for _, c := range []created{registered, unknown} {
			assert.Equal(t, c.status, http.StatusCreated)
			assert.True(t, c.token != "", "the token is shown once")
			assert.True(t, c.elapsed >= srv.signupFloor, "every answer waits for the floor")
		}
		assert.Equal(t, keys(registered.body), keys(unknown.body))
		var a, b map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(registered.body["invitation"], &a))
		require.NoError(t, json.Unmarshal(unknown.body["invitation"], &b))
		assert.Equal(t, keys(a), keys(b))
		assert.False(t, slices.Contains(keys(a), "token_hash"), "the hash never leaves the server")
		require.Equal(t, bob.do("DELETE", base+"/"+registered.id, "", nil), 204)
		require.Equal(t, bob.do("DELETE", base+"/"+unknown.id, "", nil), 204)
	})

	t.Run("old member endpoint is no probe", func(t *testing.T) {
		members := "/api/v1/accounts/orgs/" + acme.Slug + "/members"
		var known, unknown string
		assert.Equal(t, bob.do("PUT", members, `{"email":"carol@example.com","role":"member"}`, &known), 400)
		assert.Equal(t, bob.do("PUT", members, `{"email":"nobody@example.com","role":"member"}`, &unknown), 400)
		assert.Equal(t, known, unknown)
		carolUser, err := acc.St.UserByEmail(ctx, "carol@example.com")
		require.NoError(t, err)
		assert.Equal(t, bob.do("PUT", members, `{"user_id":"`+carolUser.ID+`","role":"member"}`, nil), 404, "roles change for members only")
		assert.Equal(t, bob.do("PUT", members, `{"user_id":"missing","role":"member"}`, nil), 404)
	})

	t.Run("accepting", func(t *testing.T) {
		inv := create(bob, "Carol@Example.com", "member")
		require.Equal(t, inv.status, 201)
		token := `{"token":"` + inv.token + `"}`

		var listed struct{ Invitations []accounts.Invitation }
		require.Equal(t, bob.do("GET", base, "", &listed), 200)
		require.Len(t, listed.Invitations, 1)
		assert.Equal(t, listed.Invitations[0].Email, "carol@example.com")

		var pre accounts.InvitationPreview
		require.Equal(t, dave.do("POST", "/api/v1/invitations/preview", token, &pre), 200)
		assert.False(t, pre.ForYou)
		assert.Equal(t, dave.do("POST", "/api/v1/invitations/accept", token, nil), 403, "another account's email")
		require.Equal(t, carol.do("POST", "/api/v1/invitations/preview", token, &pre), 200)
		assert.True(t, pre.ForYou)
		assert.Equal(t, pre.Org.ID, acme.ID)

		var m accounts.Membership
		require.Equal(t, carol.do("POST", "/api/v1/invitations/accept", token, &m), 200)
		assert.Equal(t, m.Role, accounts.Member)
		assert.Equal(t, carol.do("POST", "/api/v1/invitations/accept", token, nil), 404, "single use")
		assert.Equal(t, newClient(t, ts).do("POST", "/api/v1/invitations/accept", token, nil), 401)
		require.Equal(t, bob.do("GET", base, "", &listed), 200)
		assert.Len(t, listed.Invitations, 0)
		assert.Equal(t, carol.do("GET", base, "", nil), 403, "members do not see invitations")
		assert.Equal(t, create(carol, "eve@example.com", "viewer").status, 403, "members do not invite")
	})

	t.Run("revoked tokens are refused", func(t *testing.T) {
		inv := create(bob, "dave@example.com", "viewer")
		require.Equal(t, bob.do("DELETE", base+"/"+inv.id, "", nil), 204)
		assert.Equal(t, bob.do("DELETE", base+"/"+inv.id, "", nil), 404)
		assert.Equal(t, dave.do("POST", "/api/v1/invitations/accept", `{"token":"`+inv.token+`"}`, nil), 404)
		assert.Equal(t, dave.do("POST", "/api/v1/invitations/accept", `{"token":"made-up"}`, nil), 404)
	})

	t.Run("API keys neither invite nor accept", func(t *testing.T) {
		var key struct{ Key string }
		require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+acme.Slug+`","name":"agent"}`, &key), 201)
		agent := newClient(t, ts)
		agent.auth = "Bearer " + key.Key
		assert.Equal(t, create(agent, "eve@example.com", "viewer").status, 403)
		assert.Equal(t, agent.do("GET", base, "", nil), 403)

		inv := create(bob, "dave@example.com", "viewer")
		var ext accounts.Org
		require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
		var daveKey struct{ Key string }
		require.Equal(t, dave.do("POST", "/api/v1/me/keys", `{"org":"`+ext.Slug+`","name":"agent"}`, &daveKey), 201)
		daveAgent := newClient(t, ts)
		daveAgent.auth = "Bearer " + daveKey.Key
		assert.Equal(t, daveAgent.do("POST", "/api/v1/invitations/accept", `{"token":"`+inv.token+`"}`, nil), 403)
		require.Equal(t, bob.do("DELETE", base+"/"+inv.id, "", nil), 204)
	})

	t.Run("root never accepts", func(t *testing.T) {
		inv := create(bob, accountstest.RootEmail, "viewer")
		require.Equal(t, inv.status, 201)
		assert.Equal(t, rootClient(t, ts).do("POST", "/api/v1/invitations/accept", `{"token":"`+inv.token+`"}`, nil), 403)
	})

	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{OrgID: acme.ID, Action: "org.invite.accept", Outcome: accounts.Success})
	require.NoError(t, err)
	assert.Len(t, events, 1, "only carol joined")
}
