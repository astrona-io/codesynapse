package api

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPlatformAdministration(t *testing.T) {
	ts, acc := accountsServer(t)
	acc.SignupOpen = true
	ada := newClient(t, ts)
	require.Equal(t, ada.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil), 201)
	bob := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+signed.Org.Slug+`","name":"laptop"}`, nil), 201)

	for _, path := range []string{"/api/v1/admin/users", "/api/v1/admin/orgs", "/api/v1/admin/health"} {
		assert.Equal(t, bob.do("GET", path, "", nil), 403, "platform admins only: "+path)
	}
	assert.Equal(t, bob.do("POST", "/api/v1/admin/users/"+signed.User.ID+"/suspend", "", nil), 403)

	var orgs struct{ Orgs []AdminOrg }
	require.Equal(t, ada.do("GET", "/api/v1/admin/orgs", "", &orgs), 200)
	require.Len(t, orgs.Orgs, 2)
	for _, o := range orgs.Orgs {
		assert.Equal(t, o.Members, 1)
		assert.Equal(t, o.Roles["owner"], 1)
	}
	var users struct{ Users []AdminUser }
	require.Equal(t, ada.do("GET", "/api/v1/admin/users", "", &users), 200)
	require.Len(t, users.Users, 2)
	for _, u := range users.Users {
		if u.Email == "bob@example.com" {
			assert.Equal(t, u.ActiveKeys, 1)
			require.Len(t, u.Orgs, 1)
			assert.Equal(t, u.Orgs[0].Role, accounts.Owner)
		}
	}
	var raw string
	require.Equal(t, ada.do("GET", "/api/v1/admin/users", "", &raw), 200)
	assert.False(t, strings.Contains(raw, "argon2"), "no password hashes")

	// Suspending ends Bob's session at once.
	require.Equal(t, bob.do("GET", "/api/v1/me", "", nil), 200)
	require.Equal(t, ada.do("POST", "/api/v1/admin/users/"+signed.User.ID+"/suspend", "", nil), 204)
	assert.Equal(t, bob.do("GET", "/api/v1/me", "", nil), 401)
	var body string
	assert.Equal(t, bob.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"`+pw+`"}`, &body), 403)
	assert.Contains(t, body, "suspended")
	require.Equal(t, ada.do("POST", "/api/v1/admin/users/"+signed.User.ID+"/resume", "", nil), 204)
	assert.Equal(t, bob.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"`+pw+`"}`, nil), 200)

	var me struct{ User accounts.User }
	require.Equal(t, ada.do("GET", "/api/v1/me", "", &me), 200)
	assert.Equal(t, ada.do("PUT", "/api/v1/admin/users/"+me.User.ID+"/platform-admin", `{"admin":false}`, nil), 409, "the last admin stays")
	assert.Equal(t, ada.do("POST", "/api/v1/admin/users/"+me.User.ID+"/suspend", "", nil), 400, "not yourself")

	var h Health
	require.Equal(t, ada.do("GET", "/api/v1/admin/health", "", &h), 200)
	assert.True(t, h.Graph.OK && h.Accounts.OK && h.Audit.OK, "%+v", h)
	assert.False(t, h.Metrics.OK, "metrics are not configured in this test")
	assert.Equal(t, h.Counts.Users, 2)
	assert.Equal(t, h.Counts.Admins, 1)
	assert.True(t, h.Counts.AuditEvents > 0)
	assert.Len(t, h.Unowned, 0, "platform health does not disclose repository names")

	var evs struct{ Events []accounts.Event }
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit?action=user.&actor=ada@example.com", "", &evs), 200)
	actions := map[string]bool{}
	for _, e := range evs.Events {
		assert.Equal(t, e.ActorEmail, "ada@example.com")
		actions[e.Action] = true
	}
	assert.True(t, actions["user.suspend"] && actions["user.resume"])
	assert.Equal(t, ada.do("GET", "/api/v1/admin/audit?outcome=maybe", "", nil), 400)
}
