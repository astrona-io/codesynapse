package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const pw = "correct horse battery staple"

type client struct {
	t    *testing.T
	ts   *httptest.Server
	http *http.Client
	auth string
}

func newClient(t *testing.T, ts *httptest.Server) *client {
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &client{t: t, ts: ts, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path, body string, out any, headers ...string) int {
	c.t.Helper()
	req, err := http.NewRequest(method, c.ts.URL+path, strings.NewReader(body)) //nolint:noctx // test
	require.NoError(c.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.http.Do(req)
	require.NoError(c.t, err)
	defer res.Body.Close() //nolint:errcheck // test
	if out != nil {
		if s, ok := out.(*string); ok {
			b, _ := io.ReadAll(res.Body)
			*s = string(b)
		} else {
			_ = json.NewDecoder(res.Body).Decode(out)
		}
	}
	return res.StatusCode
}

func accountsServer(t *testing.T) (*httptest.Server, *accounts.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), false)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, ServiceToken: "svc-token"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	return ts, acc
}

func TestAccountsFlow(t *testing.T) {
	ts, acc := accountsServer(t)
	anon := newClient(t, ts)

	var status map[string]any
	require.Equal(t, anon.do("GET", "/api/v1/auth/status", "", &status), 200)
	assert.Equal(t, status["needs_setup"], true)
	assert.Equal(t, anon.do("GET", "/api/v1/repos", "", nil), 401, "everything else needs a login")

	ada := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, ada.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, &signed), 201)
	assert.True(t, signed.User.PlatformAdmin, "the first account administers the platform")
	var me struct {
		User        accounts.User
		Memberships []accounts.Membership
	}
	require.Equal(t, ada.do("GET", "/api/v1/me", "", &me), 200, "signing up logs in (cookie)")
	require.Len(t, me.Memberships, 1)
	assert.Equal(t, ada.do("GET", "/api/v1/repos", "", nil), 200, "platform admins see repository data")
	assert.Equal(t, anon.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 403, "sign-up closes after the first account")

	acc.SignupOpen = true
	bob := newClient(t, ts)
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 201)
	var bobRepos struct{ Repos []map[string]any }
	require.Equal(t, bob.do("GET", "/api/v1/repos", "", &bobRepos), 200)
	assert.Equal(t, len(bobRepos.Repos), 0, "ordinary users see only their organisations' repositories (none yet)")
	assert.Equal(t, bob.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil), 404)
	assert.Equal(t, bob.do("GET", "/api/v1/admin/users", "", nil), 403)
	assert.Equal(t, bob.do("GET", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/members", "", nil), 404, "other organisations are invisible")
	assert.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, nil, "Origin", "https://evil.example"), 403, "cross-origin cookie writes are refused")

	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)
	var created struct {
		Key    string
		APIKey accounts.APIKey `json:"api_key"`
	}
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop"}`, &created), 201)
	assert.True(t, strings.HasPrefix(created.Key, accounts.KeyPrefix))
	agent := newClient(t, ts)
	agent.auth = "Bearer " + created.Key
	require.Equal(t, agent.do("GET", "/api/v1/me", "", &me), 200)
	assert.Equal(t, me.User.Email, "bob@example.com")
	assert.Equal(t, agent.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"again"}`, nil), 403, "keys do not mint keys")

	assert.Equal(t, bob.do("POST", "/api/v1/auth/logout", "", nil), 204)
	assert.Equal(t, bob.do("GET", "/api/v1/me", "", nil), 401)
	assert.Equal(t, bob.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"wrong password!!"}`, nil), 401)
	require.Equal(t, bob.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"`+pw+`"}`, nil), 200)

	svc := newClient(t, ts)
	svc.auth = "Bearer svc-token"
	assert.Equal(t, svc.do("GET", "/api/v1/repos", "", nil), 200, "the service token keeps internal callers working")
	svc.auth = "Bearer wrong"
	assert.Equal(t, svc.do("GET", "/api/v1/repos", "", nil), 401)
}

func TestAuditTrail(t *testing.T) {
	ts, _ := accountsServer(t)
	ada := newClient(t, ts)
	require.Equal(t, ada.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil, "User-Agent", "audit-test/1"), 201)
	assert.Equal(t, ada.do("PUT", "/api/v1/repos/"+storetest.Repo+"/schedule", `{"bogus":true}`, nil), 404, "an inaccessible repository")
	anon := newClient(t, ts)
	assert.Equal(t, anon.do("DELETE", "/api/v1/repos/"+storetest.Repo, "", nil), 401, "a refused request")

	var out struct{ Events []accounts.Event }
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit?limit=100", "", &out), 200)
	actions := map[string]accounts.Event{}
	for _, e := range out.Events {
		actions[e.Action+" "+e.Outcome] = e
	}
	signup, ok := actions["user.signup success"]
	require.True(t, ok)
	assert.Equal(t, signup.UserAgent, "audit-test/1")
	assert.True(t, signup.RequestID != "" && signup.IP != "", "request details on every entry")
	_, ok = actions["http PUT /api/v1/repos/{repo}/schedule failure"]
	assert.True(t, ok, "failed changes are recorded with their route")
	denied, ok := actions["auth.denied failure"]
	require.True(t, ok)
	assert.Equal(t, denied.Status, 401)

	var csv string
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit?format=csv", "", &csv), 200)
	assert.True(t, strings.HasPrefix(csv, "id,at,action,outcome"))
	var v accounts.Verification
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit/verify", "", &v), 200)
	assert.True(t, v.OK, v.Reason)
}

func TestCSVSafe(t *testing.T) {
	assert.Equal(t, csvSafe("=HYPERLINK(\"x\")"), "'=HYPERLINK(\"x\")")
	assert.Equal(t, csvSafe("normal"), "normal")
}

func TestSingleTenantUnchanged(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t, ts)
	assert.Equal(t, c.do("GET", "/api/v1/repos", "", nil), 200, "without accounts the server stays open behind the outer bearer check")
	var status map[string]any
	require.Equal(t, c.do("GET", "/api/v1/auth/status", "", &status), 200)
	assert.Equal(t, status["mode"], "single")
	assert.Equal(t, c.do("POST", "/api/v1/auth/login", `{"email":"a@b.cd","password":"x"}`, nil), 409)
}

func TestClientAddrTrustsOnlyConfiguredProxies(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}}}
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	assert.Equal(t, s.clientAddr(req("172.18.0.5:4000", "203.0.113.9")), "203.0.113.9", "the web app reports the user")
	assert.Equal(t, s.clientAddr(req("172.18.0.5:4000", "1.1.1.1, 203.0.113.9")), "203.0.113.9", "the last hop is the one the proxy saw")
	assert.Equal(t, s.clientAddr(req("198.51.100.7:4000", "203.0.113.9")), "198.51.100.7", "others cannot spoof it")
	assert.Equal(t, s.clientAddr(req("172.18.0.5:4000", "not-an-ip")), "172.18.0.5")
	assert.Equal(t, s.clientAddr(req("172.18.0.5:4000", "")), "172.18.0.5")
}
