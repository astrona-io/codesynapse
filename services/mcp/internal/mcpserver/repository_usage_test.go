package mcpserver

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUsageRecordsResolvedRepository(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	rec := usage.NewRecorder(st)
	ct, transport := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, st, "test", Options{Usage: rec}).Connect(ctx, transport, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "codex"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	call(t, cs, "search", map[string]any{"query": "Handler"}, nil)
	call(t, cs, "repo_list", map[string]any{}, nil)
	require.NoError(t, rec.Flush(ctx))
	buckets, err := st.Usage(ctx, "2000-01-01")
	require.NoError(t, err)
	for _, b := range buckets {
		switch b.Tool {
		case "search":
			assert.Equal(t, b.Repo, storetest.Repo, "omitted arguments use the repository actually resolved")
		case "repo_list":
			assert.Equal(t, b.Repo, "", "workspace-wide calls remain unassigned")
		}
	}
}
