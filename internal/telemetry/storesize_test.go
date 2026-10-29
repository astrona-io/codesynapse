package telemetry

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func gaugeValue(g *Gauge, values ...string) (float64, bool) {
	key, _ := g.key(values)
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.vals[key]
	return v, ok
}

// TestStoreSizeLabelsComeFromAllowlists: only registered systems and kinds
// become series, a later sample removes kinds it no longer reports, and
// clearing removes everything.
func TestStoreSizeLabelsComeFromAllowlists(t *testing.T) {
	ctx := context.Background()
	AllowStoreKinds(DBSystemNebula, "node_test_kind")
	AllowStoreKinds("redis", "node_test_kind") // not a store system: ignored
	kept := SetStoreSizes(ctx, []StoreSize{
		{System: DBSystemNebula, Kind: "node_test_kind", Rows: 4, Bytes: -1},
		{System: DBSystemNebula, Kind: "acme/secret-repo", Rows: 9, Bytes: -1},
		{System: "redis", Kind: "node_test_kind", Rows: 1, Bytes: 1},
		{System: DBSystemPostgres, Kind: "not_registered", Rows: 1, Bytes: 1},
	})
	assert.Equal(t, kept, 1)
	v, ok := gaugeValue(StoreRows, DBSystemNebula, "node_test_kind")
	assert.True(t, ok)
	assert.Equal(t, v, 4.0)
	_, ok = gaugeValue(StoreBytes, DBSystemNebula, "node_test_kind")
	assert.False(t, ok, "rows-only stores have no bytes series")
	_, ok = gaugeValue(StoreRows, DBSystemNebula, "acme/secret-repo")
	assert.False(t, ok)
	_, ok = gaugeValue(StoreRows, "redis", "node_test_kind")
	assert.False(t, ok)

	SetStoreSizes(ctx, nil)
	_, ok = gaugeValue(StoreRows, DBSystemNebula, "node_test_kind")
	assert.False(t, ok, "a kind the next sample lacks is removed")

	SetStoreSizes(ctx, []StoreSize{{System: DBSystemNebula, Kind: "node_test_kind", Rows: 2, Bytes: 10}})
	ClearStoreSizes(ctx)
	_, ok = gaugeValue(StoreBytes, DBSystemNebula, "node_test_kind")
	assert.False(t, ok)
}
