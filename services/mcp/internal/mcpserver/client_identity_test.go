package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSessionlessClientIdentity(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	rec := usage.NewRecorder(st)
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{
		Name: "search", Arguments: json.RawMessage(`{"repo":"acme/app"}`),
		Meta: mcp.Meta{mcp.MetaKeyClientInfo: map[string]any{"name": "codex", "version": "1.0"}},
	}}
	assert.Equal(t, sessionClient(req), "codex")
	assert.Equal(t, clientName(req), "codex", "review and memory attribution use the same identity")
	assert.Equal(t, clientName(nil), "")
	handler := usageMiddleware(rec)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})
	_, err := handler(ctx, "tools/call", req)
	require.NoError(t, err)
	require.NoError(t, rec.Flush(ctx))
	buckets, err := st.Usage(ctx, "2000-01-01")
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, buckets[0].Client, "codex")
	assert.Equal(t, buckets[0].Calls, 1)
}
