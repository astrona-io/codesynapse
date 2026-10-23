package api

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestDashboardExecutionBinding(t *testing.T) {
	ts, acc := accountsServer(t)
	now := time.Now().UTC()
	acc.Now = func() time.Time { return now }
	acc.SignupOpen = true
	bob := newClient(t, ts)
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 201)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)

	// The dashboard proxy names the organisation from the cs_org cookie.
	inOrg := func(c *client) int { return c.do("GET", "/api/v1/memory/jobs", "", nil, OrgHeader, org.Slug) }
	assert.Equal(t, bob.do("GET", "/api/v1/memory/jobs", "", nil), 403, "with several organisations, one must be selected")
	assert.Equal(t, inOrg(bob), 200, "a signed-in browser gets its dashboard binding")
	assert.Equal(t, inOrg(bob), 200, "and keeps it")
	now = now.Add(accounts.ExecutionSessionTTL + time.Hour)
	assert.Equal(t, inOrg(bob), 200, "a new window opens a new binding while the sign-in lasts")
	assert.Equal(t, bob.do("GET", "/api/v1/memory/jobs", "", nil, OrgHeader, org.Slug, execution.SessionHeader, "forged", execution.ParentSessionHeader, "forged"), 403,
		"a named thread must be registered, the dashboard binding is no fallback for it")

	var created struct{ Key string }
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop"}`, &created), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + created.Key
	assert.Equal(t, inOrg(agent), 403, "API keys always name their runtime session")
	svc := newClient(t, ts)
	svc.auth = "Bearer svc-token"
	assert.Equal(t, svc.do("GET", "/api/v1/memory/jobs", "", nil), 403, "the service token never gets a binding")

	require.Equal(t, bob.do("POST", "/api/v1/auth/logout", "", nil), 204)
	assert.Equal(t, inOrg(bob), 401)
}
