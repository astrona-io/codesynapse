package storetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// successiveContract publishes one repository several times with small and
// large changes. A store may write only what changed (NebulaGraph does); the
// result must always be exactly the last graph: properties replaced and
// removed, nodes added and removed, edges changed, and edges from unchanged
// nodes into changed ones kept.
func successiveContract(t *testing.T, s CodeGraph) {
	ctx := context.Background()
	base := func() *model.Graph {
		g := Fixture()
		for i := range 100 {
			g.AddNode(model.Node{ID: ID(model.KindFunction, fmt.Sprintf("app.f%d", i)), Repo: Repo, Kind: model.KindFunction,
				Name: fmt.Sprintf("f%d", i), QualifiedName: fmt.Sprintf("app.f%d", i), File: "app/more.go", StartLine: i + 1, EndLine: i + 1})
			if i > 0 {
				g.AddEdge(model.Edge{From: ID(model.KindFunction, fmt.Sprintf("app.f%d", i-1)), To: ID(model.KindFunction, fmt.Sprintf("app.f%d", i)), Kind: model.EdgeCalls, Count: 1, Evidence: model.EvidenceTyped})
			}
		}
		return g
	}
	changed := func() *model.Graph {
		g := model.NewGraph()
		b := base()
		for _, n := range b.Nodes() {
			switch n.ID {
			case ID(model.KindFunction, "app.Handler"):
				n.Summary = "Handler serves, now with retries."
			case ID(model.KindMethod, "(*app.impl).Do"):
				n.SecurityTags = nil // a removed list property
			case ID(model.KindFunction, "app.f10"):
				continue // a removed node
			}
			g.AddNode(n)
		}
		g.AddNode(model.Node{ID: ID(model.KindFunction, "app.added"), Repo: Repo, Kind: model.KindFunction, Name: "added", QualifiedName: "app.added", File: "app/more.go", StartLine: 200, EndLine: 210})
		for _, e := range b.Edges() {
			if e.From == ID(model.KindFunction, "app.f30") {
				e.Count, e.Evidence = 3, model.EvidenceSyntactic
			}
			g.AddEdge(e) // edges into and out of app.f10 are dropped as dangling
		}
		g.AddEdge(model.Edge{From: ID(model.KindFunction, "app.f20"), To: ID(model.KindFunction, "app.added"), Kind: model.EdgeCalls, Count: 1})
		g.AddEdge(model.Edge{From: ID(model.KindFunction, "app.added"), To: ID(model.KindFunction, "app.f21"), Kind: model.EdgeCalls, Count: 2})
		return g
	}
	for i, g := range []*model.Graph{base(), changed(), changed(), base(), Fixture()} {
		require.NoError(t, s.ReplaceRepo(ctx, Repo, g), "publish %d", i)
		sameGraph(ctx, t, s, g, i)
	}
	// What other contract parts expect afterwards.
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
}

// sameGraph checks every node of want reads back unchanged, removed nodes
// are gone, and the edges between want's nodes are exactly want's.
func sameGraph(ctx context.Context, t *testing.T, s CodeGraph, want *model.Graph, round int) {
	t.Helper()
	nodes, edges, err := store.Replacement(ctx, Repo, want)
	require.NoError(t, err)
	var ids []string
	for _, n := range nodes {
		got, err := s.Node(ctx, n.ID)
		require.NoError(t, err, "round %d: node %s", round, n.ID)
		assert.Equal(t, nodeprops.To(got), nodeprops.To(n), "round %d: node %s", round, n.ID)
		ids = append(ids, n.ID)
	}
	for _, gone := range []string{ID(model.KindFunction, "app.f10"), ID(model.KindFunction, "app.added"), ID(model.KindFunction, "app.f50")} {
		if !slices.Contains(ids, gone) {
			_, err := s.Node(ctx, gone)
			assert.ErrorIs(t, err, store.ErrNotFound, "round %d: %s is gone", round, gone)
		}
	}
	type key struct {
		from, to string
		kind     model.EdgeKind
		count    int
		evidence model.Evidence
	}
	norm := func(es []model.Edge) []key {
		out := make([]key, 0, len(es))
		for _, e := range es {
			out = append(out, key{e.From, e.To, e.Kind, max(e.Count, 1), e.Evidence})
		}
		slices.SortFunc(out, func(a, b key) int {
			return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(a.to, b.to), cmp.Compare(a.kind, b.kind))
		})
		return out
	}
	got, err := s.Subgraph(ctx, ids, nil)
	require.NoError(t, err)
	assert.Equal(t, norm(got), norm(edges), "round %d: edges", round)
}
