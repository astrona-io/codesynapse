package agentctx

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func surfaceService(t *testing.T) Service {
	t.Helper()
	g := model.NewGraph()
	id := func(k model.NodeKind, local string) string { return model.NodeID("r", k, local) }
	add := func(n model.Node) string { n.Repo = "r"; g.AddNode(n); return n.ID }
	add(model.Node{ID: id(model.KindRepository, "r"), Kind: model.KindRepository, Name: "r"})
	fn := func(name string, tags ...string) string {
		return add(model.Node{ID: id(model.KindFunction, "example.com/app."+name), Kind: model.KindFunction, Name: name,
			QualifiedName: "example.com/app." + name, File: "app/app.go", StartLine: 1, EndLine: 3, SecurityTags: tags})
	}
	route := func(kind, name, guards, handler string) {
		rid := add(model.Node{ID: id(model.KindRoute, kind+" "+name+" (example.com/app)"), Kind: model.KindRoute, Name: name,
			QualifiedName: kind + " " + name + " (example.com/app)", Signature: kind, File: "app/routes.go", StartLine: 7, EndLine: 7, Summary: guards})
		g.AddEdge(model.Edge{From: rid, To: handler, Kind: model.EdgeHandledBy})
	}
	calls := func(a, b string) { g.AddEdge(model.Edge{From: a, To: b, Kind: model.EdgeCalls}) }

	run, runner := fn("run"), fn("runner", "exec")
	exe := add(model.Node{ID: id(model.KindExternal, "os/exec.Command"), Kind: model.KindExternal, Name: "Command", QualifiedName: "os/exec.Command", SecurityTags: []string{"exec"}})
	calls(run, runner)
	calls(runner, exe)
	list, open, check := fn("list"), fn("open"), fn("check", "auth")
	calls(open, check)
	route(model.RouteHTTP, "POST /run", `"admin"; withAudit`, run)
	route(model.RouteHTTP, "GET /list", "", list)
	route(model.RouteHTTP, "GET /open", "", open)
	route(model.RouteMCP, "run_tool", "", run)

	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), "r", g))
	return Service{R: s}
}

func TestAttackSurface(t *testing.T) {
	ctx := context.Background()
	s := surfaceService(t)

	all, err := s.AttackSurface(ctx, "r", SurfaceOptions{})
	require.NoError(t, err)
	assert.Equal(t, all.Total, 4)
	assert.Equal(t, all.ByKind, map[string]int{"http": 3, "mcp": 1})
	assert.Equal(t, all.Unguarded, 2, "GET /list and run_tool: GET /open checks auth")
	require.Len(t, all.EntryPoints, 4)
	first := all.EntryPoints[0]
	assert.Equal(t, first.Kind+" "+first.Name, "mcp run_tool", "unguarded and reaching exec comes first")
	require.Len(t, first.Reaches, 1)
	assert.Equal(t, first.Reaches[0], Sink{Tag: "exec", Via: "app.runner", Depth: 1})
	assert.Equal(t, first.Handler, "app.run")

	exec, err := s.AttackSurface(ctx, "r", SurfaceOptions{Reaching: "exec", Kind: "http"})
	require.NoError(t, err)
	require.Len(t, exec.EntryPoints, 1)
	assert.Equal(t, exec.EntryPoints[0].Name, "POST /run")
	assert.Equal(t, exec.EntryPoints[0].Guards, []string{`"admin"`, "withAudit"})

	open, err := s.AttackSurface(ctx, "r", SurfaceOptions{Unguarded: true, Kind: "http"})
	require.NoError(t, err)
	require.Len(t, open.EntryPoints, 1)
	assert.Equal(t, open.EntryPoints[0].Name, "GET /list")

	_, err = s.AttackSurface(ctx, "r", SurfaceOptions{Kind: "grpc"})
	assert.Error(t, err)
	_, err = s.AttackSurface(ctx, "r", SurfaceOptions{Reaching: "magic"})
	assert.Error(t, err)

	// The reverse: which entry points end up at a function.
	sc, err := s.SymbolContext(ctx, "r", "app.runner", ContextOptions{})
	require.NoError(t, err)
	assert.Equal(t, sc.Counts.EntryPoints, 2)
	require.Len(t, sc.ReachedFrom, 2)
	assert.Equal(t, sc.ReachedFrom[0].QualifiedID, "http POST /run")
	assert.Equal(t, sc.ReachedFrom[1].QualifiedID, "mcp run_tool")
}

func TestSymbolSecurityContext(t *testing.T) {
	ctx := context.Background()
	s := surfaceService(t)

	sc, err := s.SymbolContext(ctx, "r", "app.run", ContextOptions{})
	require.NoError(t, err)
	require.NotNil(t, sc.Security)
	assert.Equal(t, sc.Security.Reaches, []Sink{{Tag: "exec", Via: "app.runner", Depth: 1}})
	assert.False(t, sc.Security.ChecksAuth)
	assert.Equal(t, sc.Security.EntryPointsChecked, 2)
	assert.Equal(t, sc.Security.Unguarded, 1, "run_tool has no guard; POST /run has two")
	assert.Contains(t, sc.Security.Note, "1 of 2 entry points")
	assert.Len(t, sc.Security.EntryPoints, 0, "outline leaves the list out")

	full, err := s.SymbolContext(ctx, "r", "app.run", ContextOptions{Verbosity: Full})
	require.NoError(t, err)
	require.Len(t, full.Security.EntryPoints, 2)
	assert.Equal(t, full.Security.EntryPoints[0].Name, "POST /run")
	assert.Equal(t, full.Security.EntryPoints[0].Guards, []string{`"admin"`, "withAudit"})
	assert.Len(t, full.Security.EntryPoints[0].Reaches, 0)

	// The external call carries the tag itself.
	sc, err = s.SymbolContext(ctx, "r", "app.runner", ContextOptions{Limit: 1})
	require.NoError(t, err)
	require.NotNil(t, sc.Security)
	assert.Equal(t, sc.Security.Reaches[0], Sink{Tag: "exec", Via: "app.runner", Depth: 0})
	assert.Equal(t, sc.Security.EntryPointsChecked, 1)
	assert.Equal(t, sc.Security.Unguarded, 0, "POST /run is nearest by name and guarded")
	assert.Contains(t, sc.Security.Note, "nearest 1 of 2 entry points")

	// Nothing sensitive and no findings: no section.
	for _, sym := range []string{"app.list", "app.open"} {
		sc, err = s.SymbolContext(ctx, "r", sym, ContextOptions{})
		require.NoError(t, err)
		assert.True(t, sc.Security == nil, sym)
	}
}
