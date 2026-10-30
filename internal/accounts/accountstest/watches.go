package accountstest

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunWatches checks organisation watches: they belong to one organisation,
// a changed setting keeps the scan state, only active watches come due, and
// a scan records its result apart from the settings.
func RunWatches(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	id := execution.Identity{TenantID: "org-1", SubjectID: "user-1", SessionID: "web:1", ParentSessionID: "web:1"}
	w := accounts.OrgWatch{OrgID: "org-1", ActorID: "user-1", Execution: id, Provider: "github", Host: "github.com", Login: "Astrona-IO", NextScan: now.Add(-time.Minute)}

	t.Run("watches", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.PutOrgWatch(ctx, w))
		other := w
		other.OrgID, other.Login = "org-2", "other"
		require.NoError(t, st.PutOrgWatch(ctx, other))

		list, err := st.OrgWatches(ctx, "org-1")
		require.NoError(t, err)
		require.Equal(t, len(list), 1)
		assert.Equal(t, list[0].Key(), "github.com/astrona-io")
		assert.Equal(t, list[0].Execution, id)

		due, err := st.DueOrgWatches(ctx, now)
		require.NoError(t, err)
		assert.Equal(t, len(due), 2)

		require.NoError(t, st.RecordWatchScan(ctx, "org-1", w.Key(), accounts.WatchScan{LastScan: now, LastStatus: "ok", NextScan: now.Add(time.Hour), Added: 2, Known: []string{"a/b", "a/c"}}))
		due, err = st.DueOrgWatches(ctx, now)
		require.NoError(t, err)
		assert.Equal(t, len(due), 1, "a scanned watch waits for its next scan")

		changed := w
		changed.Paused, changed.IncludeForks = true, true
		require.NoError(t, st.PutOrgWatch(ctx, changed))
		list, err = st.OrgWatches(ctx, "org-1")
		require.NoError(t, err)
		require.Equal(t, len(list), 1)
		assert.True(t, list[0].Paused && list[0].IncludeForks)
		assert.Equal(t, list[0].Added, 2, "changing settings keeps the scan state")
		assert.Equal(t, list[0].Known, []string{"a/b", "a/c"})
		assert.Equal(t, list[0].LastStatus, "ok")

		require.NoError(t, st.RecordWatchScan(ctx, "org-1", w.Key(), accounts.WatchScan{LastStatus: "ok", Added: 1}))
		list, _ = st.OrgWatches(ctx, "org-1")
		assert.Equal(t, list[0].Added, 3)
		assert.Equal(t, list[0].Known, []string{"a/b", "a/c"}, "a scan without a seen list keeps it")
		assert.True(t, list[0].PrivateBaseline.IsZero(), "no private baseline until a scan records one")

		require.NoError(t, st.RecordWatchScan(ctx, "org-1", w.Key(), accounts.WatchScan{LastStatus: "ok", PrivateBaseline: now}))
		require.NoError(t, st.RecordWatchScan(ctx, "org-1", w.Key(), accounts.WatchScan{LastStatus: "ok"}))
		require.NoError(t, st.PutOrgWatch(ctx, changed))
		list, _ = st.OrgWatches(ctx, "org-1")
		assert.True(t, list[0].PrivateBaseline.Equal(now), "a later scan and a settings change keep the private baseline")

		require.NoError(t, st.RecordWatchScan(ctx, "org-1", "gone/none", accounts.WatchScan{}), "a removed watch is no error")
		require.NoError(t, st.DeleteOrgWatch(ctx, "org-1", w.Key()))
		list, _ = st.OrgWatches(ctx, "org-1")
		assert.Equal(t, len(list), 0)
		list, _ = st.OrgWatches(ctx, "org-2")
		assert.Equal(t, len(list), 1, "another organisation's watch is untouched")
	})
}
