package agentcfg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(s), 0o600))
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

func TestPlanResetRemovesOnlyWhatInstallAdded(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	token := filepath.Join(home, "cfg", "api-token")
	write(t, token, "csk_x\n")

	// Cursor: our server and hooks beside someone else's.
	write(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"codesynapse":{"type":"stdio"},"other":{"type":"stdio"}}}`)
	require.NoError(t, InstallLifecycleHooks(filepath.Join(home, ".cursor", "hooks.json"), "cursor", "/bin/codesynapse", "http://x"))
	write(t, filepath.Join(home, ".codex", "hooks.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"keep-me"}]}]}}`)
	require.NoError(t, InstallLifecycleHooks(filepath.Join(home, ".codex", "hooks.json"), "codex", "/bin/codesynapse", "http://x"))

	// Claude: gateway beside a foreign header, and a memory hook beside a foreign hook.
	write(t, filepath.Join(home, ".claude", "settings.json"), `{"autoCompactWindow":150000,"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:8080","ANTHROPIC_CUSTOM_HEADERS":"X-Other: 1\nX-CodeSynapse-Key: csk_x"},`+
		`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"'/bin/codesynapse' memory recall --hook"}]},{"hooks":[{"type":"command","command":"keep-me"}]}]}}`)

	// Instructions: one file holds only our block, one holds more.
	_, err := AgentsBlock(dir)
	require.NoError(t, err)
	write(t, filepath.Join(dir, "CLAUDE.md"), "# Mine\n\n"+MemoryBlock+"\nafter\n")

	actions, notes := PlanReset(ResetOptions{Home: home, Dir: dir, TokenFile: token})
	var desc []string
	for _, a := range actions {
		desc = append(desc, a.Description)
	}
	all := strings.Join(desc, "\n")
	for _, want := range []string{".cursor/mcp.json", ".cursor/hooks.json", ".codex/hooks.json", "Claude Code hooks", "gateway settings", "AGENTS.md", "CLAUDE.md", "stored key"} {
		assert.Contains(t, all, want)
	}
	assert.Contains(t, strings.Join(notes, "\n"), "context cap")

	for _, a := range actions {
		require.NoError(t, a.Apply(context.Background()), a.Description)
	}

	assert.Contains(t, read(t, filepath.Join(home, ".cursor", "mcp.json")), `"other"`)
	assert.NotContains(t, read(t, filepath.Join(home, ".cursor", "mcp.json")), Name)
	assert.NotContains(t, read(t, filepath.Join(home, ".cursor", "hooks.json")), "hook lifecycle")
	codex := read(t, filepath.Join(home, ".codex", "hooks.json"))
	assert.Contains(t, codex, "keep-me")
	assert.NotContains(t, codex, "hook lifecycle")
	claude := read(t, filepath.Join(home, ".claude", "settings.json"))
	assert.Contains(t, claude, "keep-me")
	assert.Contains(t, claude, "X-Other")
	assert.Contains(t, claude, "autoCompactWindow")
	assert.NotContains(t, claude, "memory recall")
	assert.NotContains(t, claude, "X-CodeSynapse-Key")
	assert.NotContains(t, claude, "ANTHROPIC_BASE_URL")
	_, err = os.Stat(filepath.Join(dir, "AGENTS.md"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, read(t, filepath.Join(dir, "CLAUDE.md")), "# Mine\n\nafter\n")
	_, err = os.Stat(token)
	assert.ErrorIs(t, err, os.ErrNotExist)

	again, _ := PlanReset(ResetOptions{Home: home, Dir: dir, TokenFile: token})
	assert.Len(t, again, 0, "a second reset finds nothing")
}

func TestPlanResetKeepToken(t *testing.T) {
	home := t.TempDir()
	token := filepath.Join(home, "api-token")
	write(t, token, "csk_x\n")
	actions, _ := PlanReset(ResetOptions{Home: home, Dir: t.TempDir(), TokenFile: token, KeepToken: true})
	assert.Len(t, actions, 0)
}

func TestPlanResetRefusesJSONWithComments(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".cursor", "mcp.json")
	write(t, p, `{"mcpServers":{"codesynapse":{}}} // x`)
	actions, _ := PlanReset(ResetOptions{Home: home, Dir: t.TempDir()})
	assert.Len(t, actions, 0, "unparseable files are not touched")
	assert.Equal(t, read(t, p), `{"mcpServers":{"codesynapse":{}}} // x`)
}
