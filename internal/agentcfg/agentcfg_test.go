package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestMergeJSONKeepsOtherKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"theme":"dark","mcpServers":{"other":{"command":"x"}}}`), 0o600))

	require.NoError(t, mergeJSON(p, []string{"mcpServers", Name}, map[string]any{"httpUrl": "http://localhost:8090/mcp"}))

	var got map[string]any
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, got["theme"], "dark")
	servers := got["mcpServers"].(map[string]any)
	assert.NotNil(t, servers["other"])
	assert.Equal(t, servers[Name].(map[string]any)["httpUrl"], "http://localhost:8090/mcp")

	_, err = os.Stat(p + ".bak")
	assert.NoError(t, err, "backup written")
	st, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, st.Mode().Perm(), os.FileMode(0o600))
}

func TestMergeJSONCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "mcp.json")
	require.NoError(t, mergeJSON(p, []string{"mcp", Name}, map[string]any{"url": "u"}))
	_, err := os.Stat(p)
	assert.NoError(t, err)
}

func TestMergeJSONRefusesJSONC(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.json")
	original := "{\n  // comment\n  \"a\": 1\n}\n"
	require.NoError(t, os.WriteFile(p, []byte(original), 0o600))
	assert.Error(t, mergeJSON(p, []string{"mcp", Name}, map[string]any{}))
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, string(data), original, "file untouched")
}

func TestInstallClaudeRefusesInstallationToken(t *testing.T) {
	stored := false
	s := Server{Token: "0123456789abcdef", Command: "/bin/codesynapse", StoreToken: func(string) (string, error) {
		stored = true
		return "", nil
	}}
	assert.Error(t, installClaude(t.Context(), s))
	assert.False(t, stored, "token not stored")
}

func TestDetectAlwaysOffersProject(t *testing.T) {
	targets := Detect(t.TempDir(), t.TempDir())
	ids := map[string]bool{}
	for _, tg := range targets {
		ids[tg.ID] = true
	}
	assert.True(t, ids["project"])
	assert.False(t, ids["gemini"], "no ~/.gemini in an empty home")
}
