package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestCursorSetupInstallsBridgeAndHooksWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".cursor")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	config := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(config, []byte(`{"mcpServers":{"other":{"command":"keep"}}}`), 0o600))
	stored := 0
	server := Server{Command: "/opt/bin/codesynapse", URL: "https://mcp.example.test/mcp", APIURL: "https://api.example.test", Token: "csk_personal_test", StoreToken: func(string) (string, error) { stored++; return "private", nil }}
	for _, target := range Detect(home, t.TempDir()) {
		if target.ID != "cursor" {
			continue
		}
		require.NoError(t, target.Install(t.Context(), server))
		require.NoError(t, target.Install(t.Context(), server))
	}
	assert.Equal(t, stored, 2)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	var configJSON struct {
		Servers map[string]struct {
			Type, Command string
			Args          []string
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &configJSON))
	assert.Equal(t, configJSON.Servers["other"].Command, "keep")
	assert.Equal(t, configJSON.Servers[Name].Type, "stdio")
	assert.Equal(t, configJSON.Servers[Name].Args, []string{"mcp", "bridge", "--client", "cursor", "--url", server.URL, "--api-url", server.APIURL})
	assert.False(t, strings.Contains(string(data), server.Token))
	hooks, err := os.ReadFile(filepath.Join(dir, "hooks.json"))
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(hooks), "beforeMCPExecution"))
	assert.False(t, strings.Contains(string(hooks), server.Token))
}
