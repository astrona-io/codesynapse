package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func scopedContract(t *testing.T, db store.Store) {
	ctx := context.Background()
	repo := fmt.Sprintf("scope-fixture-%d", time.Now().UnixNano())
	foreign := repo + "-foreign"
	t.Cleanup(func() { assert.NoError(t, db.DeleteRepo(ctx, repo)); assert.NoError(t, db.DeleteRepo(ctx, foreign)) })
	id := func(r, name string) string { return model.NodeID(r, model.KindFunction, name) }
	root, leaf, reentry, secret := id(repo, "root"), id(repo, "leaf"), id(repo, "reentry"), id(foreign, "secret")
	g := model.NewGraph()
	for _, name := range []string{"root", "leaf", "reentry", "branch-root", "branch-a", "branch-b", "leaf-a", "leaf-z"} {
		g.AddNode(model.Node{ID: id(repo, name), Repo: repo, Kind: model.KindFunction, Name: name})
	}
	g.AddEdge(model.Edge{From: root, To: leaf, Kind: model.EdgeCalls})
	for _, pair := range [][2]string{{"branch-root", "branch-a"}, {"branch-root", "branch-b"}, {"branch-a", "leaf-z"}, {"branch-b", "leaf-a"}} {
		g.AddEdge(model.Edge{From: id(repo, pair[0]), To: id(repo, pair[1]), Kind: model.EdgeCalls})
	}
	require.NoError(t, db.ReplaceRepo(ctx, repo, g))
	other := model.NewGraph()
	other.AddNode(model.Node{ID: secret, Repo: foreign, Kind: model.KindFunction, Name: "secret"})
	other.AddEdge(model.Edge{From: secret, To: reentry, Kind: model.EdgeCalls})
	require.NoError(t, db.ReplaceRepo(ctx, foreign, other))
	g.AddEdge(model.Edge{From: root, To: secret, Kind: model.EdgeCalls})
	require.NoError(t, db.ReplaceRepo(ctx, repo, g))
	r := store.ForRepository(db, repo)
	hits, err := r.Traverse(ctx, root, []model.EdgeKind{model.EdgeCalls}, store.Out, 3, 50)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, hits[0].Node.ID, leaf)
	assert.Equal(t, hits[0].Depth, 1)
	hits, err = r.Traverse(ctx, reentry, []model.EdgeKind{model.EdgeCalls}, store.In, 3, 50)
	require.NoError(t, err)
	assert.Len(t, hits, 0, "a path through another repository cannot reenter this one")
	hits, err = r.Traverse(ctx, leaf, []model.EdgeKind{model.EdgeCalls}, store.Out, 3, 50)
	require.NoError(t, err)
	assert.Len(t, hits, 0)
	for _, bad := range []string{secret, id(repo, "absent")} {
		_, err = r.Traverse(ctx, bad, []model.EdgeKind{model.EdgeCalls}, store.Out, 2, 50)
		assert.True(t, errors.Is(err, store.ErrNotFound))
		_, err = r.Subgraph(ctx, []string{root, bad}, nil)
		assert.True(t, errors.Is(err, store.ErrNotFound))
	}
	limited, err := r.Traverse(ctx, id(repo, "branch-root"), []model.EdgeKind{model.EdgeCalls}, store.Out, 2, 3)
	require.NoError(t, err)
	require.Len(t, limited, 3)
	assert.Equal(t, limited[2].Node.ID, id(repo, "leaf-a"), "limits apply after sorting the entire distance layer")
	es, err := r.Subgraph(ctx, []string{root, root, leaf, reentry}, nil)
	require.NoError(t, err)
	require.Len(t, es, 1)
	assert.Equal(t, es[0].From, root)
	assert.Equal(t, es[0].To, leaf)
	es, err = r.Subgraph(ctx, []string{leaf, reentry}, nil)
	require.NoError(t, err)
	assert.Len(t, es, 0)
	es, err = r.Subgraph(ctx, nil, nil)
	require.NoError(t, err)
	assert.Len(t, es, 0)
}
