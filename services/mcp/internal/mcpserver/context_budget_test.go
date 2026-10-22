package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
				Budget struct {
					MaxTokens  int `json:"max_tokens"`
					UsedTokens int `json:"used_tokens"`
				}
			}
			res := call(t, cs, tt.tool, tt.args, &out)
			require.False(t, res.IsError)
			assert.Equal(t, out.Budget.MaxTokens, 512)
			assert.LessOrEqual(t, out.Budget.UsedTokens, 512)
			encoded, err := json.Marshal(res.StructuredContent)
			require.NoError(t, err)
			assert.Equal(t, out.Budget.UsedTokens, (len(encoded)+3)/4)
		})
	}
}
