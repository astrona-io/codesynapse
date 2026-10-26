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

func TestParseContextCap(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"150k", 150_000, true},
		{"150000", 150_000, true},
		{"1M", 1_000_000, true},
		{" 200K ", 200_000, true},
		{"0", 0, true},
		{"off", 0, true},
		{"99k", 0, false},
		{"2m", 0, false},
		{"-150k", 0, false},
		{"lots", 0, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseContextCap(tc.in)
			assert.Equal(t, err == nil, tc.ok)
			assert.Equal(t, got, tc.want)
		})
	}
}

func readJSON(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // test file
	require.NoError(t, err)
	doc := map[string]any{}
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc
}

func TestSetClaudeContextCap(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"model":"opus","env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"30","KEEP":"1"}}`), 0o600))

	msg, err := SetClaudeContextCap(file, 150_000, false)
	require.NoError(t, err)
	assert.Contains(t, msg, "150k")
	assert.Contains(t, msg, CompactEnv, "the percentage would apply on top of the cap")
	doc := readJSON(t, file)
	assert.Equal[any](t, doc[claudeCapKey], float64(150_000))
	assert.Equal(t, doc["model"], "opus")
	env := doc["env"].(map[string]any)
	assert.Equal(t, env["KEEP"], "1")
	_, pct := env[CompactEnv]
	assert.False(t, pct)

	msg, err = SetClaudeContextCap(file, 200_000, false)
	require.NoError(t, err)
	assert.Contains(t, msg, "kept", "a cap already there is the user's")
	assert.Equal[any](t, readJSON(t, file)[claudeCapKey], float64(150_000))

	_, err = SetClaudeContextCap(file, 200_000, true)
	require.NoError(t, err)
	assert.Equal[any](t, readJSON(t, file)[claudeCapKey], float64(200_000))

	jsonc := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(jsonc, []byte("// mine\n{}"), 0o600))
	_, err = SetClaudeContextCap(jsonc, 150_000, true)
	require.Error(t, err)
}

func TestSetCodexContextCap(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")

	_, err := SetCodexContextCap(file, 150_000, false)
	require.NoError(t, err)
	b, _ := os.ReadFile(file) //nolint:gosec // test file
	assert.Equal(t, string(b), "model_auto_compact_token_limit = 150000\n")

	existing := "model = \"gpt-5\"\n\n[mcp_servers.codesynapse]\ncommand = \"codesynapse\"\nmodel_auto_compact_token_limit = 1\n"
	require.NoError(t, os.WriteFile(file, []byte(existing), 0o600))
	_, err = SetCodexContextCap(file, 150_000, false)
	require.NoError(t, err)
	b, _ = os.ReadFile(file) //nolint:gosec // test file
	assert.True(t, strings.HasPrefix(string(b), "model_auto_compact_token_limit = 150000\nmodel = \"gpt-5\"\n"), "added at the top level, not inside a table")
	assert.Contains(t, string(b), "[mcp_servers.codesynapse]\ncommand = \"codesynapse\"\nmodel_auto_compact_token_limit = 1\n")

	msg, err := SetCodexContextCap(file, 200_000, false)
	require.NoError(t, err)
	assert.Contains(t, msg, "kept")
	_, err = SetCodexContextCap(file, 200_000, true)
	require.NoError(t, err)
	b, _ = os.ReadFile(file) //nolint:gosec // test file
	assert.Equal(t, strings.Count(string(b), "model_auto_compact_token_limit = 200000"), 1)
	assert.Contains(t, string(b), "model_auto_compact_token_limit = 1\n", "the table's own key is untouched")
}
