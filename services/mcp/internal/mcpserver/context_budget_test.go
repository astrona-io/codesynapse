package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCompositeToolBudgets(t *testing.T) {
	cs := connect(t)
	for _, tt := range []struct {
		tool string
		args map[string]any
	}{
		{"get_symbol_context", map[string]any{"symbol": "app.Handler", "verbosity": "full"}},
		{"get_call_graph", map[string]any{"symbol": "app.Handler", "direction": "both"}},
		{"get_pr_context", map[string]any{"files": []string{"app/app.go"}}},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			tt.args["max_tokens"] = 512
			var out struct {
				Budget *struct {
					MaxTokens  int `json:"max_tokens"`
					UsedTokens int `json:"used_tokens"`
					Omitted    int `json:"omitted"`
				}
			}
			res := call(t, cs, tt.tool, tt.args, &out)
			require.False(t, res.IsError)
			encoded, err := json.Marshal(res.StructuredContent)
			require.NoError(t, err)
			assert.LessOrEqual(t, (len(encoded)+3)/4, 512, "within max_tokens either way")
			// The block is there only when something had to be left out,
			// and then it describes the answer it is part of.
			if out.Budget != nil {
				assert.Equal(t, out.Budget.MaxTokens, 512)
				assert.Greater(t, out.Budget.Omitted, 0)
				assert.LessOrEqual(t, out.Budget.UsedTokens, 512)
			}
		})
	}
}
