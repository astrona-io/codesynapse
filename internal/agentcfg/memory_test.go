package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/memory/extract"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAgentsBlock(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Rules\n\nBe nice.\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("Claude notes\n"), 0o600))
	changed, err := AgentsBlock(dir)
	require.NoError(t, err)
	assert.Equal(t, len(changed), 2, "CLAUDE.md without @AGENTS.md gets the block too")
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	assert.True(t, strings.HasPrefix(string(b), "# Rules\n\nBe nice.\n\n"+blockStart), "existing text is kept")

	changed, err = AgentsBlock(dir)
	require.NoError(t, err)
	assert.Equal(t, len(changed), 0, "re-running changes nothing")
	again, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	assert.Equal(t, strings.Count(string(again), blockStart), 1)

	// An outdated block is replaced in place.
	old := strings.Replace(string(again), "Use it without being asked", "old text", 1) + "Footer\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(old), 0o600))
	_, err = AgentsBlock(dir)
	require.NoError(t, err)
	b, _ = os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	assert.True(t, strings.Contains(string(b), "Use it without being asked") && strings.HasSuffix(string(b), "Footer\n"))

	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "CLAUDE.md"), []byte("@AGENTS.md\n"), 0o600))
	changed, err = AgentsBlock(other)
	require.NoError(t, err)
	assert.Equal(t, changed, []string{filepath.Join(other, "AGENTS.md")}, "created; CLAUDE.md already imports it")
}

func TestInstallHooks(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(`{"model":"opus","env":{"FOO":"1"},"hooks":{"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"say compacting"}]}]}}`), 0o600))
	cmds := HookCommands{SessionStart: "codesynapse memory recall --hook --repo r", PreCompact: "codesynapse memory offload --hook --repo r", CompactAt: 30}
	require.NoError(t, InstallHooks(p, cmds))
	require.NoError(t, InstallHooks(p, cmds))

	var doc struct {
		Model string
		Env   map[string]string
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	b, _ := os.ReadFile(p)
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, doc.Model, "opus")
	assert.Equal(t, doc.Env, map[string]string{"FOO": "1", CompactEnv: "30"}, "other variables are kept")
	require.Error(t, InstallHooks(p, HookCommands{CompactAt: 101}))
	require.Len(t, doc.Hooks["PreCompact"], 2, "the user's own hook stays; ours is not duplicated")
	assert.Equal(t, doc.Hooks["PreCompact"][0].Hooks[0].Command, "say compacting")
	assert.Equal(t, doc.Hooks["SessionStart"][0].Hooks[0].Command, cmds.SessionStart)
	fi, _ := os.Stat(p)
	assert.Equal(t, fi.Mode().Perm(), os.FileMode(0o600))
}

func TestReadTranscript(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"summary","summary":"old"}`,
		`{"type":"user","message":{"role":"user","content":"We should drop Redis."}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Agreed."},{"type":"tool_use","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"` + strings.Repeat("x", 3000) + `"}]}]}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
		`not json`,
		`{"type":"user","message":{"role":"user","content":"Use Postgres instead."}}`,
	}, "\n")
	turns, err := ReadTranscript(strings.NewReader(lines))
	require.NoError(t, err)
	require.Len(t, turns, 4)
	assert.Equal(t, turns[0], extract.Turn{Role: "user", Text: "We should drop Redis."})
	assert.Equal(t, turns[1], extract.Turn{Role: "assistant", Text: "Agreed."})
	assert.Equal(t, turns[2].Role, "tool")
	assert.True(t, len([]rune(turns[2].Text)) <= maxToolText+2, "tool output shortened")

	assert.Equal(t, len(OlderThanWindow(turns, 1)), 3, "everything before the last user turn")
	assert.Equal(t, len(OlderThanWindow(turns, 2)), 0, "two exchanges fit the window")
}
