package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// runtimeContract: measured facts round-trip per namespace, a new upload
// replaces the old one whole, organisations sharing a graph never see each
// other's facts, and deleting the graph removes every namespace's.
func runtimeContract(t *testing.T, s store.Store) {
	ctx := context.Background()
	rs, ok := s.(store.RuntimeStore)
	require.True(t, ok, "every store keeps runtime facts")
	const graph = "runtime-graph"
	acme, globex := "org-acme|"+graph, "org-globex|"+graph

	_, err := rs.RuntimeFacts(ctx, acme)
	assert.True(t, errors.Is(err, store.ErrNotFound))

	// Enough distinct IDs that the compressed document spans several chunks.
	big := runtimefacts.Facts{Commit: "abc123", MeasuredAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), Origin: "upload",
		Sources: []string{"coverage cover.out"}, Unit: "statements", Coverage: map[string]runtimefacts.Coverage{}, Tests: map[string]runtimefacts.TestRun{}}
	for i := range 6000 {
		h := sha256.Sum256(fmt.Appendf(nil, "%d", i))
		big.Coverage[graph+"|Function|pkg.F"+hex.EncodeToString(h[:12])] = runtimefacts.Coverage{Covered: i % 7, Total: 7}
	}
	big.Tests[graph+"|Function|pkg.TestF"] = runtimefacts.TestRun{Passed: 2, Failed: 1}
	require.NoError(t, rs.SetRuntimeFacts(ctx, acme, big))
	got, err := rs.RuntimeFacts(ctx, acme)
	require.NoError(t, err)
	assert.Equal(t, len(got.Coverage), 6000)
	assert.Equal(t, got.Commit, "abc123")
	assert.True(t, got.MeasuredAt.Equal(big.MeasuredAt))
	assert.True(t, got.Tests[graph+"|Function|pkg.TestF"].Flaky())

	small := runtimefacts.Facts{Commit: "def456", Origin: "upload", Coverage: map[string]runtimefacts.Coverage{graph + "|Function|pkg.G": {Covered: 1, Total: 2}}}
	require.NoError(t, rs.SetRuntimeFacts(ctx, acme, small))
	got, err = rs.RuntimeFacts(ctx, acme)
	require.NoError(t, err)
	assert.Equal(t, got.Commit, "def456", "a new upload replaces the old one")
	assert.Equal(t, len(got.Coverage), 1, "nothing of the old upload is left")

	_, err = rs.RuntimeFacts(ctx, globex)
	assert.True(t, errors.Is(err, store.ErrNotFound), "another organisation on the same graph sees nothing")

	// RuntimeFactsOf reads the caller's scope, and only for its graph.
	f, ok, err := store.RuntimeFactsOf(store.WithRuntimeScope(ctx, acme), s, graph)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, f.Commit, "def456")
	_, ok, err = store.RuntimeFactsOf(store.WithRuntimeScope(ctx, globex), s, graph)
	require.NoError(t, err)
	assert.False(t, ok)
	_, ok, err = store.RuntimeFactsOf(store.WithRuntimeScope(ctx, acme), s, Repo)
	require.NoError(t, err)
	assert.False(t, ok, "a scope for another graph finds nothing")
	_, ok, err = store.RuntimeFactsOf(store.WithRuntimeScope(ctx, acme), store.ForRepository(s, Repo), graph)
	require.NoError(t, err)
	assert.False(t, ok, "a reader fenced to another repository finds nothing")

	require.NoError(t, rs.SetRuntimeFacts(ctx, globex, small))
	require.NoError(t, rs.DeleteRuntimeFacts(ctx, globex))
	_, err = rs.RuntimeFacts(ctx, globex)
	assert.True(t, errors.Is(err, store.ErrNotFound))

	require.NoError(t, rs.SetRuntimeFacts(ctx, globex, small))
	require.NoError(t, s.DeleteRepo(ctx, graph))
	for _, ns := range []string{acme, globex} {
		_, err = rs.RuntimeFacts(ctx, ns)
		assert.True(t, errors.Is(err, store.ErrNotFound), "deleting the graph removes "+ns)
	}
}
