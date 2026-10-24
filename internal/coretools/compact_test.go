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
	got, err := compact(context.Background(), memory.New(), "acme/app", in, nil)
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

// The root Go module is written as "~" in compact IDs, and such an ID
// resolves in the next call.
func TestModuleRelativeIDs(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	id := model.NodeID("acme/app", model.KindMethod, "(*example.com/app/store.Store).Get")
	g.AddNode(model.Node{ID: model.NodeID("acme/app", model.KindRepository, "acme/app"), Kind: model.KindRepository, Repo: "acme/app", Name: "acme/app", Meta: `{"go_module":"example.com/app"}`})
	g.AddNode(model.Node{ID: id, Kind: model.KindMethod, Repo: "acme/app", Name: "Get", QualifiedName: "(*store.Store).Get", File: "store/store.go", StartLine: 1, EndLine: 3})
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "acme/app", g))
	out, err := FileOutline(ctx, mem, "acme/app", FileIn{File: "store/store.go"})
	require.NoError(t, err)
	require.Len(t, out.Entries, 1)
	assert.Equal(t, out.Entries[0].ID, "Method|(*~/store.Store).Get")
	n, err := agentctx.Service{R: mem}.Resolve(ctx, "acme/app", out.Entries[0].ID)
	require.NoError(t, err)
	assert.Equal(t, n.ID, id)
}

// Conventions cost minimal_context's matches part of their budget, so they
// come only when asked for; a subagent's brief always has them.
func TestConventionsOnRequest(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("acme/app", model.KindRepository, "acme/app"), Kind: model.KindRepository, Repo: "acme/app", Name: "acme/app",
		Meta: `{"conventions":["errors: fmt.Errorf (3 calls) and errors.New (1)"]}`})
	g.AddNode(model.Node{ID: model.NodeID("acme/app", model.KindFunction, "example.com/app.Run"), Kind: model.KindFunction, Repo: "acme/app", Name: "Run", QualifiedName: "example.com/app.Run", File: "app.go", StartLine: 1, EndLine: 3})
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "acme/app", g))
	plain, err := MinimalContext(ctx, mem, "acme/app", MinimalContextIn{Task: "change Run"})
	require.NoError(t, err)
	assert.Equal(t, len(plain.Conventions), 0)
	asked, err := MinimalContext(ctx, mem, "acme/app", MinimalContextIn{Task: "change Run", Conventions: true})
	require.NoError(t, err)
	assert.Equal(t, asked.Conventions, []string{"errors: fmt.Errorf (3 calls) and errors.New (1)"})
	brief, err := Handoff(ctx, mem, nil, "acme/app", HandoffIn{Task: "change Run"}, nil)
	require.NoError(t, err)
	assert.Contains(t, brief.Brief, "House style")
	assert.Contains(t, brief.Brief, "- errors: fmt.Errorf (3 calls) and errors.New (1)")
}
