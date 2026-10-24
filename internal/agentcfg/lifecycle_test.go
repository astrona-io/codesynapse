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

func TestLifecycleInstallerPreservesOtherHooks(t *testing.T) {
	for _, client := range []string{"codex", "cursor"} {
		t.Run(client, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "hooks.json")
			event := HookEvents(client)[0]
			other := map[string]any{"command": "other-tool"}
			if client == "codex" {
				other = map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "other-tool"}}}
			}
			original, err := json.Marshal(map[string]any{"description": "keep me", "hooks": map[string]any{event: []any{other}}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(p, original, 0o600))
			require.NoError(t, InstallLifecycleHooks(p, client, "/opt/Code Synapse/bin/agent'one", "https://example.com/api?x=$(secret)"))
			backup, err := os.ReadFile(p + ".bak")
			require.NoError(t, err)
			assert.Equal(t, string(backup), string(original))
			first, err := os.ReadFile(p)
			require.NoError(t, err)
			require.NoError(t, InstallLifecycleHooks(p, client, "/opt/Code Synapse/bin/agent'one", "https://example.com/api?x=$(secret)"))
			second, err := os.ReadFile(p)
			require.NoError(t, err)
			assert.Equal(t, string(first), string(second), "repeat installation is idempotent")
			var doc struct {
				Description string
				Hooks       map[string][]any
			}
			require.NoError(t, json.Unmarshal(second, &doc))
			assert.Equal(t, doc.Description, "keep me")
			assert.Len(t, doc.Hooks, len(HookEvents(client)))
			assert.Len(t, doc.Hooks[event], 2)
			assert.True(t, strings.Contains(string(second), "other-tool"))
		})
	}
}

func TestLifecycleInstallerRefusesMalformedSettings(t *testing.T) {
	for _, input := range []string{"null", "[]", `{"hooks":[]}`, `{"hooks":{"Stop":{}}}`, `{"hooks":{"Stop":[{"command":"unexpected"}]}}`} {
		t.Run(input, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "hooks.json")
			require.NoError(t, os.WriteFile(p, []byte(input), 0o600))
			require.Error(t, InstallLifecycleHooks(p, "codex", "/bin/codesynapse", "https://example.com"))
			got, err := os.ReadFile(p)
			require.NoError(t, err)
			assert.Equal(t, string(got), input)
		})
	}
}
