package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func memoryCountsContract(t *testing.T, s store.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	names := []string{"000000000000000000000001|count-a", "000000000000000000000001|count-b", "000000000000000000000002|count-a", "legacy-count"}
	t.Cleanup(func() {
		for _, ns := range names {
			for _, name := range []string{"Service", "Old team", "New team"} {
				_ = s.DeleteMemoryEntity(ctx, ns, memory.EntityID(ns, name))
			}
		}
	})
	put := func(ns, team string, replace bool) {
		t.Helper()
		b, err := memory.Prepare(ns, []memory.Triple{{Subject: "Service", Predicate: "OWNED_BY", Object: team, Replace: replace}}, "test", time.Now())
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, b))
	}
	for _, ns := range names {
		put(ns, "Old team", false)
		put(ns, "Old team", false)
	}
	put(names[0], "New team", true)
	read := func() map[string]int64 {
		t.Helper()
		counts := map[string]int64{}
		after := ""
		for {
			page, err := s.MemoryFactCounts(ctx, after, 1)
			require.NoError(t, err)
			if len(page) == 0 {
				return counts
			}
			require.Len(t, page, 1)
			require.True(t, page[0].Namespace > after, "pagination must advance")
			counts[page[0].Namespace] = page[0].Facts
			after = page[0].Namespace
		}
	}
	counts := read()
	for _, ns := range names {
		assert.Equal(t, counts[ns], int64(1), "duplicates and replaced history are not current facts")
	}
	g, err := s.SearchMemory(ctx, names[0], "", 10, false)
	require.NoError(t, err)
	require.Len(t, g.Facts, 1)
	require.NoError(t, s.DeleteMemoryFact(ctx, names[0], g.Facts[0].ID))
	counts = read()
	zero, present := counts[names[0]]
	assert.True(t, present, "namespace with surviving entities reports zero")
	assert.Equal(t, zero, int64(0))
	assert.Equal(t, counts[names[1]], int64(1), "deletion stays in its namespace")
	require.NoError(t, s.DeleteMemoryEntity(ctx, names[2], memory.EntityID(names[2], "Service")))
	assert.Equal(t, read()[names[2]], int64(0), "entity deletion removes its durable facts")
}
