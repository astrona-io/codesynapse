package agentcfg

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestClaudeGatewaySetAndRemove(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	require.NoError(t, os.WriteFile(file, []byte(`{"theme":"dark","env":{"FOO":"1","ANTHROPIC_CUSTOM_HEADERS":"X-Other: a"}}`), 0o600))
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	diff, err := SetClaudeGateway(file, "http://127.0.0.1:8080/", "csk_secretsecret", now)
	require.NoError(t, err)
	assert.True(t, strings.Contains(diff, "+ env.ANTHROPIC_BASE_URL: http://127.0.0.1:8080"))
	assert.False(t, strings.Contains(diff, "secretsecret"), "key masked in the diff")
	got, _ := os.ReadFile(file)
	assert.True(t, strings.Contains(string(got), `X-Other: a\nX-CodeSynapse-Key: csk_secretsecret`))
	assert.True(t, strings.Contains(string(got), `"theme": "dark"`))
	bak, _ := os.ReadFile(file + ".20261027T120000Z.bak")
	assert.True(t, strings.Contains(string(bak), `"FOO":"1"`), "timestamped backup of the old file")

	// Running again with a new key replaces only our header.
	_, err = SetClaudeGateway(file, "http://127.0.0.1:8080", "csk_otherotherkey", now.Add(time.Second))
	require.NoError(t, err)
	got, _ = os.ReadFile(file)
	assert.Equal(t, strings.Count(string(got), "X-CodeSynapse-Key"), 1)

	_, err = RemoveClaudeGateway(file, now.Add(2*time.Second))
	require.NoError(t, err)
	got, _ = os.ReadFile(file)
	assert.False(t, strings.Contains(string(got), "ANTHROPIC_BASE_URL"))
	assert.False(t, strings.Contains(string(got), "X-CodeSynapse-Key"))
	assert.True(t, strings.Contains(string(got), "X-Other: a"), "other headers stay")
	assert.True(t, strings.Contains(string(got), `"FOO"`))

	msg, err := RemoveClaudeGateway(file, now.Add(3*time.Second))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(msg, "nothing to remove"))
}

func TestClaudeGatewayRefusals(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mine := filepath.Join(dir, "own.json")
	require.NoError(t, os.WriteFile(mine, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example"}}`), 0o600))
	_, err := SetClaudeGateway(mine, "http://localhost:8080", "csk_abcdefghij", now)
	assert.Error(t, err, "a base URL we did not write is kept")
	got, _ := os.ReadFile(mine)
	assert.True(t, strings.Contains(string(got), "proxy.example"))
	_, err = RemoveClaudeGateway(mine, now)
	require.NoError(t, err)
	got, _ = os.ReadFile(mine)
	assert.True(t, strings.Contains(string(got), "proxy.example"), "remove leaves a URL we did not write")

	_, err = SetClaudeGateway(filepath.Join(dir, "a.json"), "http://gateway.example", "csk_abcdefghij", now)
	assert.Error(t, err, "plain http to another host")
	_, err = SetClaudeGateway(filepath.Join(dir, "b.json"), "https://gateway.example", "sk-ant-nope", now)
	assert.Error(t, err, "not a CodeSynapse key")
}

func TestCheckGateway(t *testing.T) {
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/v1/models")
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer csk_k")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	for _, c := range []struct {
		status int
		ok     bool
	}{{200, true}, {503, true}, {404, false}, {401, false}, {500, false}} {
		status = c.status
		err := CheckGateway(t.Context(), nil, srv.URL, "csk_k")
		assert.Equal(t, err == nil, c.ok, "status ", c.status)
	}
	assert.Error(t, CheckGateway(t.Context(), nil, "http://127.0.0.1:1", "csk_k"))
}
