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
