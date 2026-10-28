package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

func TestGatewayForwardsWithKeyAndBindsSession(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	key := strings.TrimPrefix(f.client.auth, "Bearer ")

	call := func(headers map[string]string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{"model":"m"}`)) //nolint:noctx // test
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	status, body := call(map[string]string{gateway.KeyHeader: key, "X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 200)
	assert.Equal(t, body, `{"ok":true}`)
	assert.Equal(t, seen.Get("X-Api-Key"), "sk-ant-own")
	assert.Equal(t, seen.Get(gateway.KeyHeader), "")

	status, _ = call(map[string]string{"X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 401, "no gateway key")
	status, _ = call(map[string]string{gateway.KeyHeader: "csk_nope", "X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 401, "unknown gateway key")
	status, _ = call(map[string]string{gateway.KeyHeader: key})
	assert.Equal(t, status, 401, "no model credential")

	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "execution_session.register", Limit: 100})
	require.NoError(t, err)
	gw := 0
	for _, e := range rows {
		if strings.HasPrefix(e.Target, "gw:") {
			gw++
		}
	}
	assert.Equal(t, gw, 1, "one audited gateway session")
}

func TestGatewayAuditsCallsWithoutPrompts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer up.Close()
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"TOP-SECRET-PROMPT"}]}`)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
	req.Header.Set("X-Api-Key", "sk-ant-own")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, res.StatusCode, 200)

	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.request", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].Detail, "route=cloud")
	assert.Contains(t, rows[0].Detail, "model=claude-x")
	assert.NotContains(t, rows[0].Detail, "TOP-SECRET")
	assert.NotContains(t, rows[0].Detail, "sk-ant-own")
}

func TestGatewayCapsOpenRequestsPerKey(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		held.Add(1)
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	defer close(release)
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	post := func() int {
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{}`)) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
		req.Header.Set("X-Api-Key", "sk-ant-own")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	for range maxGatewayInFlight {
		go post()
	}
	deadline := time.Now().Add(5 * time.Second)
	for held.Load() < maxGatewayInFlight && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int(held.Load()), maxGatewayInFlight)
	assert.Equal(t, post(), http.StatusTooManyRequests)
}

func TestGatewayOffByDefault(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader("{}")) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
	req.Header.Set("X-Api-Key", "k")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	assert.Equal(t, res.StatusCode, 404)
}

func TestGatewayOpenAIBearerKeyAndNoLocalModel(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	do := func(method, path, auth string) int {
		req, err := http.NewRequest(method, f.client.ts.URL+path, strings.NewReader(`{"model":"x","messages":[]}`)) //nolint:noctx // test
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	assert.Equal(t, do(http.MethodGet, "/v1/models", ""), 401)
	assert.Equal(t, do(http.MethodGet, "/v1/models", "Bearer csk_nope"), 401)
	assert.Equal(t, do(http.MethodGet, "/v1/models", f.client.auth), 503, "valid key, no local model")
	assert.Equal(t, do(http.MethodPost, "/v1/chat/completions", f.client.auth), 503)
}

func TestGatewayOrganisationCanSwitchItOff(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	status := func() int {
		req, err := http.NewRequest(http.MethodGet, f.client.ts.URL+"/v1/models", nil) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Authorization", f.client.auth)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	assert.Equal(t, status(), 503, "on by default, no local model")

	path := "/api/v1/accounts/orgs/" + f.org.Slug + "/gateway"
	var got struct {
		Enabled   bool `json:"enabled"`
		Available bool `json:"available"`
	}
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.True(t, got.Enabled)
	assert.True(t, got.Available)
	assert.Equal(t, f.client.do("PUT", path, `{"enabled":false}`, nil), 403, "API keys cannot switch it off")
	assert.Equal(t, status(), 503)

	require.NoError(t, f.accounts.St.SetGatewayEnabled(t.Context(), f.org.ID, false))
	assert.Equal(t, status(), 403, "switched off for the organisation")
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.False(t, got.Enabled)
}

func TestGatewayLocalFirstSetting(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	path := "/api/v1/accounts/orgs/" + f.org.Slug + "/gateway"
	type setting struct {
		Enabled    bool `json:"enabled"`
		LocalFirst bool `json:"local_first"`
	}
	var got setting
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true}, "both on by default")
	assert.Equal(t, f.client.do("PUT", path, `{"local_first":false}`, nil), 403, "API keys cannot change it")

	owner := newClient(t, f.client.ts)
	require.Equal(t, owner.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	assert.Equal(t, owner.do("PUT", path, `{}`, nil), 400, "nothing to change")
	require.Equal(t, owner.do("PUT", path, `{"local_first":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: false}, "enabled is left alone")
	require.Equal(t, owner.do("PUT", path, `{"enabled":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: false, LocalFirst: false}, "older clients send only enabled")
	require.Equal(t, owner.do("PUT", path, `{"enabled":true,"local_first":true}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true})
}
