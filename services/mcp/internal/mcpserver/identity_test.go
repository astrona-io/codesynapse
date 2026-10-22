package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/parisnakitakejser/codesynapse/internal/agentctx"
	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/identity"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestIdentityToolsAndSharedCatalog(t *testing.T) {
	ctx := context.Background()
	db := memory.New()
	g := storetest.Fixture()
	r, err := identity.Reconcile(g, storetest.Repo, identity.Report{Version: 1, Repo: storetest.Repo})
	require.NoError(t, err)
	rn, _ := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	rn.Identities, err = identity.Encode(r)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceRepo(ctx, storetest.Repo, g))
	ct, st := mcp.NewInMemoryTransports()
	_, err = New(db, "test", Options{Curated: db, CatalogWrites: true}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "codex-test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	var page query.IdentityPage
	require.False(t, call(t, cs, "code_identities", map[string]any{"limit": 1}, &page).IsError)
	assert.True(t, page.Available)
	assert.True(t, page.Truncated)
	require.Len(t, page.Symbols, 1)
	id := page.Symbols[0].ID
	var context agentctx.SymbolContext
	require.False(t, call(t, cs, "get_symbol_context", map[string]any{"symbol": id}, &context).IsError)
	assert.Equal(t, context.Symbol.SymbolID, id)
	var note catalog.CodeView
	require.False(t, call(t, cs, "code_annotate", map[string]any{"symbol": id, "purpose": "Entry point", "labels": []string{"api"}}, &note).IsError)
	assert.Equal(t, note.Item.Client, "codex-test")
	var records catalog.CodePage
	require.False(t, call(t, cs, "code_records", map[string]any{"symbol": id}, &records).IsError)
	require.Len(t, records.Records, 1)
	shared, err := (catalog.Service{R: db, Items: db}).CodeRecords(ctx, storetest.Repo, id, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, records, shared, "MCP and API use the same catalogue result")
	assert.True(t, call(t, cs, "code_identities", map[string]any{"repo": "foreign"}, nil).IsError)
	assert.True(t, call(t, cs, "code_annotate", map[string]any{"symbol": "sym-00000000000000000000000000000000", "purpose": "invalid target"}, nil).IsError)
}
