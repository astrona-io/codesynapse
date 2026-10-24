package coretools

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCompactIDs(t *testing.T) {
	in := RefsOut{Results: []query.Ref{
		{ID: "acme/app|Function|example.com/app.Run", Kind: model.KindFunction, Name: "Run", Summary: "acme/app|not an id"},
		{ID: "other/repo|Function|example.com/x.Y", Kind: model.KindFunction, Depth: 2},
	}}
	got, err := compact("acme/app", in, nil)
	require.NoError(t, err)
	assert.Equal(t, got.Results[0].ID, "Function|example.com/app.Run", "this repository's prefix goes")
	assert.Equal(t, got.Results[0].Summary, "acme/app|not an id", "text that is not an ID stays")
	assert.Equal(t, got.Results[1].ID, "other/repo|Function|example.com/x.Y", "other repositories keep theirs")
	assert.Equal(t, got.Results[1].Depth, 2)
}

// A compact ID from one answer resolves in the next call.
func TestCompactIDsResolve(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	id := model.NodeID("acme/app", model.KindFunction, "example.com/app.Run")
	g.AddNode(model.Node{ID: model.NodeID("acme/app", model.KindRepository, "acme/app"), Kind: model.KindRepository, Repo: "acme/app", Name: "acme/app"})
	g.AddNode(model.Node{ID: id, Kind: model.KindFunction, Repo: "acme/app", Name: "Run", QualifiedName: "example.com/app.Run", File: "app.go", StartLine: 1, EndLine: 3})
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "acme/app", g))
	found, err := Search(ctx, mem, "acme/app", SearchIn{Query: "Run"})
	require.NoError(t, err)
	require.Len(t, found.Results, 1)
	assert.Equal(t, found.Results[0].ID, "Function|example.com/app.Run")
	n, err := agentctx.Service{R: mem}.Resolve(ctx, "acme/app", found.Results[0].ID)
	require.NoError(t, err)
	assert.Equal(t, n.ID, id)
}
