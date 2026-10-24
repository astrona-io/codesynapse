package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEntryPointsEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g := model.NewGraph()
	id := func(k model.NodeKind, local string) string { return model.NodeID("r", k, local) }
	g.AddNode(model.Node{ID: id(model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	h := id(model.KindFunction, "app.run")
	g.AddNode(model.Node{ID: h, Kind: model.KindFunction, Repo: "r", Name: "run", QualifiedName: "app.run", File: "a.go", StartLine: 1, EndLine: 3, SecurityTags: []string{"exec"}})
	rt := id(model.KindRoute, "http POST /run (app)")
	g.AddNode(model.Node{ID: rt, Kind: model.KindRoute, Repo: "r", Name: "POST /run", QualifiedName: "http POST /run (app)", Signature: "http", File: "a.go", StartLine: 9, EndLine: 9})
	g.AddEdge(model.Edge{From: rt, To: h, Kind: model.EdgeHandledBy})
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got agentctx.AttackSurface
	require.Equal(t, get(t, ts, "/api/v1/repos/r/entry-points?reaching=exec", &got), 200)
	require.Len(t, got.EntryPoints, 1)
	assert.Equal(t, got.EntryPoints[0].Name, "POST /run")
	assert.Equal(t, got.Unguarded, 1)
	assert.Equal(t, get(t, ts, "/api/v1/repos/r/entry-points?kind=grpc", nil), 400)

	var sec Sections
	require.Equal(t, get(t, ts, "/api/v1/repos/r/sections", &sec), 200)
	assert.Equal(t, sec.Routes, 1)
}
