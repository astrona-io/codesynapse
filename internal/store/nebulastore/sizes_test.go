package nebulastore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestSizeStatementsAreConstant: nothing is formatted into the sampler's
// statements; the one value travels as $now.
func TestSizeStatementsAreConstant(t *testing.T) {
	for _, stmt := range []string{leasesHeld, submitStats, showJobs, showStats} {
		assert.False(t, strings.Contains(stmt, "%"), "format verb in %q", stmt)
		assert.Equal(t, strings.Count(stmt, "$"), strings.Count(stmt, "$now"), "parameters of %q", stmt)
	}
}

// TestStatsSizesKeepOwnKinds: rows named after anything but this store's
// tags and edge types (space totals, a tag someone else created) are
// dropped, and every kept kind is one the gauges allow.
func TestStatsSizesKeepOwnKinds(t *testing.T) {
	rows := []Row{
		{"Type": "Tag", "Name": "node", "Count": int64(12)},
		{"Type": "Edge", "Name": "CALLS", "Count": int64(7)},
		{"Type": "Tag", "Name": "secret_customer_tag", "Count": int64(3)},
		{"Type": "Space", "Name": "vertices", "Count": int64(12)},
		{"Type": "Edge", "Name": "CALLS", "Count": int64(7)},
		{"Type": "Tag", "Name": "review", "Count": int64(-1)},
	}
	got := statsSizes(rows)
	require.Equal(t, len(got), 2)
	assert.Equal(t, got[0], telemetry.StoreSize{System: telemetry.DBSystemNebula, Kind: "CALLS", Rows: 7, Bytes: -1})
	assert.Equal(t, got[1].Kind, "node")
	for _, it := range schemaItems {
		assert.True(t, telemetry.StoreKindAllowed(telemetry.DBSystemNebula, it.name), "%s is not allowed", it.name)
	}
	assert.False(t, telemetry.StoreKindAllowed(telemetry.DBSystemNebula, "secret_customer_tag"))
}

func TestJobStatus(t *testing.T) {
	rows := []Row{{"Job Id": int64(3), "Status": "FINISHED"}, {"Job Id": int64(4), "Status": "running"}}
	assert.Equal(t, jobStatus(rows, 3), "FINISHED")
	assert.Equal(t, jobStatus(rows, 4), "RUNNING")
	assert.Equal(t, jobStatus(rows, 5), "")
}

// TestSizesWaitForReplaceLease: while a publication holds its lease the
// sampler answers ErrSizesBusy and starts no job; afterwards it counts.
func TestSizesWaitForReplaceLease(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	head := vid("h", "sizes-lease-test")
	require.NoError(t, s.lease(ctx, head, "sizes-lease-test", "run-1", time.Minute))
	_, err := s.StoreSizes(ctx)
	assert.True(t, errors.Is(err, store.ErrSizesBusy), "got %v", err)
	require.NoError(t, s.release(ctx, head, "run-1"))
	sizes, err := s.StoreSizes(ctx)
	require.NoError(t, err)
	assert.True(t, len(sizes) > 0)
}
