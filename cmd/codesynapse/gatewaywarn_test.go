package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func writeClaudeSettings(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600))
}

func TestGatewayWarning(t *testing.T) {
	const key = "csk_secretsecret"
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer "+key)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	conf := `{"env":{"ANTHROPIC_BASE_URL":"` + srv.URL + `","ANTHROPIC_CUSTOM_HEADERS":"X-CodeSynapse-Key: ` + key + `"}}`

	t.Run("no settings file", func(t *testing.T) {
		assert.Equal(t, gatewayWarningWith(t.Context(), t.TempDir(), gatewayproxy.Files{}), "")
	})
	t.Run("base URL not ours", func(t *testing.T) {
		home := t.TempDir()
		writeClaudeSettings(t, home, `{"env":{"ANTHROPIC_BASE_URL":"`+srv.URL+`"}}`)
		assert.Equal(t, gatewayWarningWith(t.Context(), home, gatewayproxy.Files{}), "")
	})
	t.Run("gateway healthy", func(t *testing.T) {
		home := t.TempDir()
		writeClaudeSettings(t, home, conf)
		status = http.StatusServiceUnavailable // no local model still counts as up
		assert.Equal(t, gatewayWarningWith(t.Context(), home, gatewayproxy.Files{}), "")
	})
	t.Run("gateway refuses", func(t *testing.T) {
		home := t.TempDir()
		writeClaudeSettings(t, home, conf)
		status = http.StatusUnauthorized
		w := gatewayWarningWith(t.Context(), home, gatewayproxy.Files{})
		assert.True(t, strings.Contains(w, "mcp install --gateway --remove"))
		assert.False(t, strings.Contains(w, key), "the key never reaches the warning")
	})
	t.Run("gateway down", func(t *testing.T) {
		home := t.TempDir()
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		writeClaudeSettings(t, home, `{"env":{"ANTHROPIC_BASE_URL":"`+url+`","ANTHROPIC_CUSTOM_HEADERS":"X-CodeSynapse-Key: `+key+`"}}`)
		assert.True(t, strings.Contains(gatewayWarningWith(t.Context(), home, gatewayproxy.Files{}), "not answering"))
	})
	t.Run("behind the failover proxy", func(t *testing.T) {
		home := t.TempDir()
		url := deadGateway(t)
		files := gatewayproxy.Files{Dir: t.TempDir()}
		cfg := gatewayproxy.Config{Gateway: url, Port: gatewayproxy.DefaultPort}
		require.NoError(t, files.Save(cfg))
		writeClaudeSettings(t, home, `{"env":{"ANTHROPIC_BASE_URL":"`+cfg.URL()+`","ANTHROPIC_CUSTOM_HEADERS":"X-CodeSynapse-Key: `+key+`"}}`)
		w := gatewayWarningWith(t.Context(), home, files)
		assert.Contains(t, w, url, "the gateway behind the proxy is checked")
		assert.Contains(t, w, "straight to Anthropic")
	})
}

func deadGateway(t *testing.T) string {
	t.Helper()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	return dead.URL
}
