package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/catalog"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	_, err = newTestServer(t, db, "test", Options{Curated: db, CatalogWrites: true}).Connect(ctx, st, nil)
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

func TestMCPReadsHumanReviewedIdentity(t *testing.T) {
	ctx := context.Background()
	db := memory.New()
	original := storetest.Fixture()
	first, err := identity.Reconcile(original, storetest.Repo, identity.Report{Repo: storetest.Repo, Version: 1})
	require.NoError(t, err)
	from, to := first.Symbols[0].ID, first.Symbols[1].ID
	next := model.NewGraph()
	for _, n := range original.Nodes() {
		if n.SymbolID == from {
			continue
		}
		if n.Kind == model.KindRepository {
			n.Head = "next"
		}
		next.AddNode(n)
	}
	for _, e := range original.Edges() {
		if _, ok := next.Node(e.From); ok {
			if _, ok := next.Node(e.To); ok {
				next.AddEdge(e)
			}
		}
	}
	report, err := identity.Reconcile(next, storetest.Repo, first)
	require.NoError(t, err)
	rn, _ := next.Node(storetest.ID(model.KindRepository, storetest.Repo))
	rn.Identities, err = identity.Encode(report)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceRepo(ctx, storetest.Repo, next))
	_, err = (catalog.Service{R: db, Items: db}).ReviewIdentity(ctx, storetest.Repo, catalog.Actor{User: "human"}, curation.IdentityReviewInput{From: from, To: to, Relation: "same-symbol", Reason: "Reviewed source", Revision: report.Revision, IndexedAt: report.IndexedAt})
	require.NoError(t, err)
	ct, st := mcp.NewInMemoryTransports()
	_, err = newTestServer(t, db, "test", Options{Curated: db}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "identity-reader"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	var reviews catalog.IdentityReviewPage
	require.False(t, call(t, cs, "code_identity_reviews", map[string]any{}, &reviews).IsError)
	assert.Equal(t, reviews.Total, 1)
	var result agentctx.SymbolContext
	require.False(t, call(t, cs, "get_symbol_context", map[string]any{"symbol": from}, &result).IsError)
	assert.Equal(t, result.Symbol.SymbolID, to)
	listed, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	for _, tool := range listed.Tools {
		if tool.Name == "code_identity_reviews" {
			assert.True(t, tool.Annotations.ReadOnlyHint)
		}
	}
}

func TestCrossRepositoryReuseRechecksLiveSession(t *testing.T) {
	url, keys, acc, _, db := keyServer(t)
	ctx := context.Background()
	stamp := func(repo string, g *model.Graph) identity.Report {
		t.Helper()
		r, err := identity.Reconcile(g, repo, identity.Report{Version: 1, Repo: repo})
		require.NoError(t, err)
		rn, _ := g.Node(model.NodeID(repo, model.KindRepository, repo))
		rn.Identities, err = identity.Encode(r)
		require.NoError(t, err)
		require.NoError(t, db.ReplaceRepo(ctx, repo, g))
		return r
	}
	destination := stamp(storetest.Repo, storetest.Fixture())
	source := stamp("private-source", archGraph(t, "private-source", map[string]string{}))
	owner, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: owner.OrgID, Graph: "private-source", AddedBy: owner.User.ID, AddedAt: time.Now()}))
	cs, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	args := map[string]any{"repo": storetest.Repo, "origin_repo": "private-source", "method": "copy", "origins": []string{source.Symbols[0].ID}, "destinations": []string{destination.Symbols[0].ID}, "reason": "source-private rationale", "compatibility": "unknown", "license": "unknown", "attribution": "unknown", "validation": "not run"}
	var created catalog.CodeView
	require.False(t, call(t, cs, "code_record_reuse", args, &created).IsError)
	assert.Equal(t, created.Item.Code.Sources[0].Repo, "private-source")
	assert.Equal(t, created.Item.OrgID, owner.OrgID)
	var page catalog.CodePage
	require.False(t, call(t, cs, "code_records", map[string]any{"repo": storetest.Repo}, &page).IsError)
	assert.Len(t, page.Records, 1)
	other, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	assert.True(t, call(t, other, "code_record_reuse", args, nil).IsError, "a key cannot borrow source permissions from another organisation")
	_, err = acc.St.RemoveRepository(ctx, owner.OrgID, "private-source")
	require.NoError(t, err)
	require.False(t, call(t, cs, "code_records", map[string]any{"repo": storetest.Repo}, &page).IsError)
	assert.Len(t, page.Records, 0)
	assert.True(t, page.SourcesRestricted)
	assert.Equal(t, page.Total, 0)
	assert.True(t, call(t, cs, "code_record_reuse", args, nil).IsError, "resumed sessions recheck source access")
	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{})
	require.NoError(t, err)
	found := false
	denied := false
	for _, event := range events {
		found = found || event.Action == "mcp.code.record_reuse" && event.UserID == owner.User.ID && event.OrgID == owner.OrgID
		denied = denied || event.Action == "mcp.access_denied" && event.Target == "code_record_reuse" && event.UserID == owner.User.ID
	}
	assert.True(t, found, "the attributed provenance write is audited")
	assert.True(t, denied, "revoked source access is an audited denial")
}
