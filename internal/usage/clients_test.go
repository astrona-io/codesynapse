package usage

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestClientFamily(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code": "claude", "Claude Code": "claude", "codex": "codex", "codex-mcp-client": "codex",
		"gemini-cli": "gemini", "opencode": "opencode", "cursor-vscode": "cursor",
		"": ClientUnknown, "unknown": ClientUnknown, "my-editor": ClientOther,
	} {
		assert.Equal(t, ClientFamily(in), want, in)
	}
	assert.Equal(t, len(ClientFamilies), 7)
}
