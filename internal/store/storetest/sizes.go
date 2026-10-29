package storetest

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sizesContract: a store reports at least the fixture's nodes, every kind
// once, and only kinds of its own schema (the gauge allowlist), never
// anything taken from stored data.
func sizesContract(t *testing.T, s store.Store) {
	t.Helper()
	sz, ok := s.(store.Sizer)
	require.True(t, ok, "every store reports its sizes")
	sizes, err := sz.StoreSizes(context.Background())
	require.NoError(t, err)
	seen := map[[2]string]bool{}
	var nodes int64
	for _, x := range sizes {
		k := [2]string{x.System, x.Kind}
		assert.False(t, seen[k], "kind reported twice: %v", k)
		seen[k] = true
		assert.True(t, x.Rows >= 0, "rows of %v", k)
		if x.System != store.System(s) && x.System != telemetry.DBSystemMilvus {
			t.Errorf("size of another system: %v", k)
		}
		if x.System == telemetry.DBSystemNebula || x.System == telemetry.DBSystemMilvus {
			assert.True(t, telemetry.StoreKindAllowed(x.System, x.Kind), "kind outside the allowlist: %v", k)
		}
		if x.Kind == "node" {
			nodes = x.Rows
		}
	}
	assert.True(t, nodes >= int64(len(Fixture().Nodes())), "node rows %d, fixture has %d", nodes, len(Fixture().Nodes()))
}
