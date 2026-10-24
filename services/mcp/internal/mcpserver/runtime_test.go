package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestMeasuredFactsFollowTheCaller: get_symbol_context reads the test run of
// the caller's namespace (memRepo: the graph without accounts, "org|graph"
// with them), never another organisation's.
func TestMeasuredFactsFollowTheCaller(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	handler := storetest.ID(model.KindFunction, "app.Handler")
	facts := func(covered int) runtimefacts.Facts {
		return runtimefacts.Facts{Commit: "abc", Unit: "statements", Coverage: map[string]runtimefacts.Coverage{handler: {Covered: covered, Total: 4}}}
	}
	require.NoError(t, s.SetRuntimeFacts(ctx, agentmem.Namespace("org-other", storetest.Repo), facts(4)))

	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test").Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	var out agentctx.SymbolContext
	call(t, cs, "get_symbol_context", map[string]any{"symbol": "app.Handler"}, &out)
	assert.True(t, out.Measured == nil, "another organisation's run is not the caller's")

	require.NoError(t, s.SetRuntimeFacts(ctx, storetest.Repo, facts(1)))
	call(t, cs, "get_symbol_context", map[string]any{"symbol": "app.Handler"}, &out)
	require.NotNil(t, out.Measured)
	assert.Equal(t, out.Measured.Coverage, "1 of 4 statements covered (25%)")
}
