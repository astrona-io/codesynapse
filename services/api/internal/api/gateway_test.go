package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
