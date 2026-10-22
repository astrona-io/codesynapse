package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type foreignEdgeReader struct {
	store.Reader
	expanded []string
}

func (r *foreignEdgeReader) Traverse(_ context.Context, id string, _ []model.EdgeKind, _ store.Direction, _, _ int) ([]model.Hit, error) {
	r.expanded = append(r.expanded, id)
	if id == "a|Function|root" {
		return []model.Hit{{Node: model.Node{ID: "b|Function|secret", Repo: "b"}, Depth: 1}, {Node: model.Node{ID: "a|Function|leaf", Repo: "a"}, Depth: 1}}, nil
	}
	return nil, nil
}

func TestRepositoryReader(t *testing.T) {
	ctx := context.Background()
	db := memory.New()
	for _, repo := range []string{"a", "b"} {
		g := model.NewGraph()
		for _, name := range []string{"root", "leaf", "secret"} {
			g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, name), Repo: repo, Kind: model.KindFunction, Name: name})
		}
		require.NoError(t, db.ReplaceRepo(ctx, repo, g))
	}
	backend := &foreignEdgeReader{Reader: db}
	r := store.ForRepository(backend, "a")
	_, err := r.Node(ctx, "b|Function|secret")
	assert.True(t, errors.Is(err, store.ErrNotFound))
	_, err = r.Search(ctx, "b", "secret", nil, 10)
	assert.True(t, errors.Is(err, store.ErrNotFound))
	_, err = r.Subgraph(ctx, []string{"a|Function|root", "b|Function|secret"}, nil)
	assert.True(t, errors.Is(err, store.ErrNotFound))
	hits, err := r.Traverse(ctx, "a|Function|root", []model.EdgeKind{model.EdgeCalls}, store.Out, 3, 50)
	require.NoError(t, err)
	require.Equal(t, len(hits), 1)
	assert.Equal(t, hits[0].Node.Repo, "a")
	assert.Equal(t, backend.expanded, []string{"a|Function|root", "a|Function|leaf"})
	_, err = store.ForRepository(r, "b").Node(ctx, "b|Function|secret")
	assert.True(t, errors.Is(err, store.ErrNotFound), "wrapping cannot widen scope")
}

type nativeGraphReader struct {
	store.Reader
	calls int
	repo  string
	hits  []model.Hit
	edges []model.Edge
}

func (r *nativeGraphReader) TraverseRepository(_ context.Context, repo, _ string, _ []model.EdgeKind, _ store.Direction, _, _ int) ([]model.Hit, error) {
	r.calls++
	r.repo = repo
	return r.hits, nil
}

func (r *nativeGraphReader) SubgraphRepository(_ context.Context, repo string, _ []string, _ []model.EdgeKind) ([]model.Edge, error) {
	r.calls++
	r.repo = repo
	return r.edges, nil
}

func TestRepositoryReaderRejectsForeignNativeResults(t *testing.T) {
	root, leaf := "a|Function|root", "a|Function|leaf"
	backend := &nativeGraphReader{hits: []model.Hit{
		{Node: model.Node{ID: "b|Function|secret", Repo: "b"}, Depth: 1},
		{Node: model.Node{ID: "b|Function|forged", Repo: "a"}, Depth: 1},
		{Node: model.Node{ID: root, Repo: "a"}, Depth: 0},
		{Node: model.Node{ID: leaf, Repo: "a"}, Depth: 1},
		{Node: model.Node{ID: leaf, Repo: "a"}, Depth: 2},
		{Node: model.Node{ID: "a|Function|deep", Repo: "a"}, Depth: 3},
	}, edges: []model.Edge{
		{From: root, To: leaf, Kind: model.EdgeCalls},
		{From: root, To: "b|Function|secret", Kind: model.EdgeCalls},
		{From: root, To: "a|Function|unrequested", Kind: model.EdgeCalls},
		{From: root, To: leaf, Kind: model.EdgeImports},
	}}
	r := store.ForRepository(backend, "a")
	hits, err := r.Traverse(t.Context(), root, []model.EdgeKind{model.EdgeCalls}, store.Out, 2, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, hits[0].Node.ID, leaf)
	es, err := r.Subgraph(t.Context(), []string{root, leaf}, []model.EdgeKind{model.EdgeCalls})
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, es[0].To, leaf)
}

func TestRepositoryReaderUsesNativeBoundary(t *testing.T) {
	ctx := context.Background()
	// A nil embedded Reader would panic if the wrapper made per-node probes.
	backend := &nativeGraphReader{}
	r := store.ForRepository(backend, "a")
	_, err := r.Traverse(ctx, "a|Function|root", []model.EdgeKind{model.EdgeCalls}, store.Out, 3, 50)
	require.NoError(t, err)
	assert.Equal(t, backend.calls, 1)
	assert.Equal(t, backend.repo, "a")
	_, err = r.Subgraph(ctx, []string{"a|Function|root", "a|Function|leaf"}, nil)
	require.NoError(t, err)
	assert.Equal(t, backend.calls, 2)
	_, err = r.Subgraph(ctx, []string{"a|Function|root", "b|Function|secret"}, nil)
	assert.True(t, errors.Is(err, store.ErrNotFound))
	assert.Equal(t, backend.calls, 2, "foreign selectors are rejected before querying")
}
