package agentcfg

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestClaudeGatewayProxyRoundTrip(t *testing.T) {
	const (
		proxy  = "http://127.0.0.1:18431"
		gw     = "https://gw.example.com"
		key    = "csk_secretsecret"
		ensure = "'/opt/homebrew/opt/codesynapse/bin/codesynapse' gateway proxy --ensure"
	)
	file := filepath.Join(t.TempDir(), ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	orig := `{"theme":"dark","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"other-tool start"}]}]}}`
	require.NoError(t, os.WriteFile(file, []byte(orig), 0o600))
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hooksOf := func() map[string]any {
		t.Helper()
		doc, _, err := readSettings(file)
		require.NoError(t, err)
		h, _ := doc["hooks"].(map[string]any)
		return h
	}

	_, err := SetClaudeGatewayProxy(file, proxy, key, "rm -rf /", now)
	require.Error(t, err, "only the ensure command may be added as a hook")

	diff, err := SetClaudeGatewayProxy(file, proxy, key, ensure, now)
	require.NoError(t, err)
	assert.Contains(t, diff, "hooks.SessionStart")
	assert.NotContains(t, diff, "secretsecret")
	assert.Len(t, hooksOf()["SessionStart"], 2, "other hooks stay")
	assert.Len(t, hooksOf()["UserPromptSubmit"], 1)

	// Installing again does not add the hooks twice.
	_, err = SetClaudeGatewayProxy(file, proxy, key, ensure, now.Add(time.Second))
	require.NoError(t, err)
	assert.Len(t, hooksOf()["SessionStart"], 2)

	// Fail open to the gateway and back to the proxy.
	changed, err := PointClaudeGateway(file, proxy, gw, gw)
	require.NoError(t, err)
	assert.True(t, changed)
	base, _, _ := ClaudeGatewayConfig(file)
	assert.Equal(t, base, gw)
	changed, err = PointClaudeGateway(file, proxy, gw, gw)
	require.NoError(t, err)
	assert.False(t, changed, "already there")
	changed, err = PointClaudeGateway(file, proxy, gw, proxy)
	require.NoError(t, err)
	assert.True(t, changed)
	_, err = PointClaudeGateway(file, proxy, gw, "https://elsewhere.example.com")
	require.Error(t, err)

	// --failover=false: the old setup, without the hooks.
	diff, err = SetClaudeGateway(file, gw, key, now.Add(2*time.Second))
	require.NoError(t, err)
	assert.Contains(t, diff, "- hooks")
	assert.Len(t, hooksOf()["SessionStart"], 1)
	_, ok := hooksOf()["UserPromptSubmit"]
	assert.False(t, ok)

	// --remove takes out the env and the hooks, nothing else.
	_, err = SetClaudeGatewayProxy(file, proxy, key, ensure, now.Add(3*time.Second))
	require.NoError(t, err)
	_, err = RemoveClaudeGateway(file, now.Add(4*time.Second))
	require.NoError(t, err)
	doc, _, err := readSettings(file)
	require.NoError(t, err)
	_, hasEnv := doc["env"]
	assert.False(t, hasEnv)
	assert.Equal(t, doc["theme"], any("dark"))
	assert.Len(t, hooksOf()["SessionStart"], 1)
	_, ok = hooksOf()["UserPromptSubmit"]
	assert.False(t, ok)
}

func TestPointClaudeGatewayLeavesOthersAlone(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:18431"}}`), 0o600))
	changed, err := PointClaudeGateway(file, "http://127.0.0.1:18431", "https://gw.example.com", "https://gw.example.com")
	require.NoError(t, err)
	assert.False(t, changed, "without our header the base URL is someone else's")
}
