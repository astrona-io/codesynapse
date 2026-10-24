package golang

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestRoutes(t *testing.T) {
	root, err := filepath.Abs("testdata/routes")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "routes", Root: root})
	require.NoError(t, err)
	g := res.Graph

	id := func(k model.NodeKind, local string) string { return model.NodeID("routes", k, local) }
	method := func(name string) string { return id(model.KindMethod, "(*example.com/routes/app.Server)."+name) }
	fn := func(name string) string { return id(model.KindFunction, "example.com/routes/app."+name) }
	handled := map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	type route struct{ kind, name, handler, guards string }
	got := map[string]route{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			got[n.Signature+" "+n.Name] = route{n.Signature, n.Name, handled[n.ID], n.Summary}
			assert.Equal(t, n.Package, "example.com/routes/app")
			assert.Greater(t, n.StartLine, 0)
		}
	}
	want := map[string]route{
		"http GET /items": {"http", "GET /items", method("listItems"), `"viewer"`},
		"http POST /run":  {"http", "POST /run", method("run"), `"admin"; withAudit`},
		"http /open":      {"http", "/open", "", ""}, // inline: no function of the graph serves it
		"http GET /self":  {"http", "GET /self", method("ServeHTTP"), ""},
		"http GET /made":  {"http", "GET /made", method("factory"), ""},
		"mcp search":      {"mcp", "search", fn("search"), ""},
		"mcp lookup":      {"mcp", "lookup", fn("lookup"), ""},
		"cli serve":       {"cli", "serve", fn("serve"), ""},
	}
	assert.Equal(t, got, want, "filepath.Walk, the wrapper's own registration and a command that runs nothing are no routes")
}

func TestRouterMiddleware(t *testing.T) {
	root, err := filepath.Abs("testdata/routers")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "routers", Root: root})
	require.NoError(t, err)
	got := map[string]string{}
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindRoute {
			got[n.Name] = n.Summary
		}
	}
	assert.Equal(t, got, map[string]string{
		"GET /health":      "", // registered before Use
		"GET /items":       "requireAuth",
		"GET /admin/stats": "requireAuth; adminOnly",
		"GET /users":       "requireAuth; adminOnly",
		"GET /api/me":      "requireAuth",
		"GET /public/open": "",
		"/v1/things":       "requireAuth",
		"GET /wrapped":     "requireAuth",
	}, "logger is middleware but no access check")
}
