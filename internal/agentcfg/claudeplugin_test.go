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

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
}

func TestClaudePluginEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		want             bool
	}{
		{"user settings", ".claude/settings.json", `{"enabledPlugins":{"codesynapse@codesynapse":true}}`, true},
		{"other marketplace", ".claude/settings.json", `{"enabledPlugins":{"codesynapse@astrona":true}}`, true},
		{"disabled", ".claude/settings.json", `{"enabledPlugins":{"codesynapse@codesynapse":false}}`, false},
		{"another plugin", ".claude/settings.json", `{"enabledPlugins":{"codesynapse-extra@x":true,"other@codesynapse":true}}`, false},
		{"project", "project/.claude/settings.local.json", `{"enabledPlugins":{"codesynapse@codesynapse":true}}`, true},
		{"not JSON", ".claude/settings.json", `{"enabledPlugins":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeFile(t, filepath.Join(home, tc.file), tc.body)
			assert.Equal(t, ClaudePluginEnabled(home, filepath.Join(home, "project")), tc.want)
		})
	}
	assert.False(t, ClaudePluginEnabled(t.TempDir(), ""), "no settings")
}

const oldHooks = `{"model":"opus","hooks":{
 "SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"'/opt/codesynapse' memory recall --hook --repo 'r'"}]}],
 "PreCompact":[
  {"matcher":"","hooks":[{"type":"command","command":"say compacting"},{"type":"command","command":"codesynapse memory offload --hook --repo r"}]}],
 "Stop":[{"hooks":[{"type":"command","command":"'/opt/codesynapse' hook lifecycle --client claude --event Stop --api-url 'u'"}]}],
 "PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"gofmt -w"}]}]}}`

func TestRemoveClaudeHooks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, p, oldHooks)

	n, err := RemoveClaudeHooks(p)
	require.NoError(t, err)
	assert.Equal(t, n, 3)

	var doc struct {
		Model string
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, doc.Model, "opus", "other settings kept")
	assert.Equal(t, len(doc.Hooks), 2, "events left empty are dropped")
	require.Len(t, doc.Hooks["PreCompact"], 1)
	assert.Equal(t, doc.Hooks["PreCompact"][0].Hooks[0].Command, "say compacting", "a shared group keeps the user's handler")
	assert.Equal(t, doc.Hooks["PostToolUse"][0].Hooks[0].Command, "gofmt -w")
	assert.False(t, hasClaudeHooks(p))

	n, err = RemoveClaudeHooks(p)
	require.NoError(t, err)
	assert.Equal(t, n, 0, "nothing left to remove")
	n, err = RemoveClaudeHooks(filepath.Join(t.TempDir(), "missing.json"))
	require.NoError(t, err)
	assert.Equal(t, n, 0)
}

func TestRemoveClaudeHooksRefusesJSONC(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, p, "{\n // comment\n}\n")
	_, err := RemoveClaudeHooks(p)
	assert.Error(t, err)
}

// With the plugin, setup removes what an earlier install wrote instead of
// adding a second server and second hooks, and leaves the shared project
// settings to the team.
func TestCleanClaudeForPlugin(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	user := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, user, strings.Replace(oldHooks, `"model":"opus"`, `"model":"opus","enabledPlugins":{"codesynapse@codesynapse":true}`, 1))
	local := filepath.Join(cwd, ".claude", "settings.local.json")
	writeFile(t, local, oldHooks)
	shared := filepath.Join(cwd, ".claude", "settings.json")
	writeFile(t, shared, oldHooks)

	var notes []string
	stored := ""
	s := Server{
		Token: "csk_personal", ContextCap: DefaultContextCap,
		StoreToken: func(tok string) (string, error) { stored = tok; return "", nil },
		Note:       func(m string) { notes = append(notes, m) },
	}
	require.NoError(t, cleanClaudeForPlugin(t.Context(), home, cwd, s))
	require.NoError(t, setClaudeCap(home, s))

	assert.Equal(t, stored, "csk_personal", "the plugin's bridge reads the stored key")
	assert.False(t, hasClaudeHooks(user))
	assert.False(t, hasClaudeHooks(local))
	assert.True(t, hasClaudeHooks(shared), "shared settings are only reported")
	require.Len(t, notes, 4)
	assert.Contains(t, notes[2], "settings.json (shared)")
	assert.Contains(t, notes[3], "150k")
	assert.True(t, ClaudePluginEnabled(home, cwd), "the plugin stays enabled")
}
