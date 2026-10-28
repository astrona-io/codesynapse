package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// historyContract checks HistoryStore: snapshots come back oldest first, the
// same time replaces, old ones are dropped past the cap, repositories do not
// see each other's, and replacing or deleting the repository's graph leaves
// them alone (only DeleteSnapshots removes them).
func historyContract(t *testing.T, db interface {
	store.Writer
	store.HistoryStore
}) {
	ctx := context.Background()
	repo := fmt.Sprintf("hist-fixture-%d", time.Now().UnixNano())
	other := repo + "-other"
	t.Cleanup(func() {
		assert.NoError(t, db.DeleteSnapshots(ctx, repo))
		assert.NoError(t, db.DeleteSnapshots(ctx, other))
		assert.NoError(t, db.DeleteRepo(ctx, repo))
	})
	at := func(i int) string { return time.Date(2026, 10, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339) }
	snap := func(i int, grade string) history.Snapshot {
		return history.Snapshot{At: at(i), Head: fmt.Sprintf("c%d", i), Branch: fmt.Sprintf("b%d", i%2), Grade: grade, Score: 90 - i, Findings: map[string]int{"high": i}, DupPct: 1.5}
	}

	got, err := db.Snapshots(ctx, repo, 10)
	require.NoError(t, err)
	assert.Len(t, got, 0, "no history yet")

	for _, i := range []int{3, 1, 2} {
		require.NoError(t, db.AddSnapshot(ctx, repo, snap(i, "A")))
	}
	require.NoError(t, db.AddSnapshot(ctx, other, snap(1, "F")))
	got, err = db.Snapshots(ctx, repo, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, got[0].At, at(1), "oldest first")
	assert.Equal(t, got[2].At, at(3))
	assert.Equal(t, got[2].Findings["high"], 3)
	assert.Equal(t, got[1].DupPct, 1.5)
	assert.Equal(t, got[0].Branch, "b1", "the branch comes back, so trends can be read per branch")
	assert.Equal(t, got[1].Branch, "b0")

	require.NoError(t, db.AddSnapshot(ctx, repo, snap(2, "B")))
	got, err = db.Snapshots(ctx, repo, 10)
	require.NoError(t, err)
	require.Len(t, got, 3, "the same time replaces")
	assert.Equal(t, got[1].Grade, "B")

	got, err = db.Snapshots(ctx, repo, 2)
	require.NoError(t, err)
	require.Len(t, got, 2, "the latest two, still oldest first")
	assert.Equal(t, got[0].At, at(2))

	o, err := db.Snapshots(ctx, other, 10)
	require.NoError(t, err)
	require.Len(t, o, 1)
	assert.Equal(t, o[0].Grade, "F")

	// The graph's own lifecycle does not touch the history.
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Repo: repo, Kind: model.KindRepository, Name: repo})
	require.NoError(t, db.ReplaceRepo(ctx, repo, g))
	require.NoError(t, db.DeleteRepo(ctx, repo))
	got, err = db.Snapshots(ctx, repo, 10)
	require.NoError(t, err)
	assert.Len(t, got, 3)

	require.NoError(t, db.DeleteSnapshots(ctx, repo))
	got, err = db.Snapshots(ctx, repo, 10)
	require.NoError(t, err)
	assert.Len(t, got, 0)
	o, err = db.Snapshots(ctx, other, 10)
	require.NoError(t, err)
	assert.Len(t, o, 1, "another repository's history stays")
}
