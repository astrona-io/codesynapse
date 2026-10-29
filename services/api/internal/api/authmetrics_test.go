package api

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Sign-in attempts and audit writes are counted with fixed labels; the
// tenant is the one the server resolved, never what the request names.
func TestAuthAndAuditMetrics(t *testing.T) {
	ts, acc := accountsServer(t)
	acc.SignupOpen = true
	bob := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	var made struct {
		Key string
	}
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+signed.Org.Slug+`","name":"laptop"}`, &made), 201)
	require.True(t, made.Key != "", "the key is returned once")

	anon := newClient(t, ts)
	assert.Equal(t, anon.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"wrong-password-1"}`, nil), 401)
	assert.Equal(t, anon.do("GET", "/api/v1/repos", "", nil), 401, "no credential: not an attempt")
	agent := newClient(t, ts)
	agent.auth = "Bearer " + made.Key
	assert.Equal(t, agent.do("GET", "/api/v1/repos", "", nil), 200)
	agent.auth = "Bearer " + accounts.KeyPrefix + "not-a-key"
	assert.Equal(t, agent.do("GET", "/api/v1/repos", "", nil), 401)

	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	out := b.String()
	assert.Contains(t, out, `platform_codesynapse_auth_attempts_total{method="password",outcome="bad_credentials"`)
	assert.Contains(t, out, `codesynapse_auth_attempts_total{method="api_key",outcome="ok",tenant_id="`+signed.Org.ID+`"`)
	assert.Contains(t, out, `platform_codesynapse_auth_attempts_total{method="api_key",outcome="bad_credentials"`)
	assert.Contains(t, out, `codesynapse_audit_writes_total{source="api",outcome="ok"`)
	assert.False(t, strings.Contains(out, "bob@example.com"))
}
