package api

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// sizedStore answers fixed sizes, or ErrSizesBusy while busy is set.
type sizedStore struct {
	*memory.Store
	busy  bool
	calls int
}

func (s *sizedStore) StoreSizes(context.Context) ([]telemetry.StoreSize, error) {
	s.calls++
	if s.busy {
		return nil, store.ErrSizesBusy
	}
	return []telemetry.StoreSize{{System: telemetry.DBSystemNebula, Kind: "node", Rows: 42, Bytes: -1}}, nil
}

// TestStoreSamplerSkipsWhileReplaceLeaseHeld: a held replace lease
// publishes nothing and retries soon; the next try samples once, and
// another process waits for the next day.
func TestStoreSamplerSkipsWhileReplaceLeaseHeld(t *testing.T) {
	ctx := t.Context()
	telemetry.ClearStoreSizes(ctx)
	t.Cleanup(func() { telemetry.ClearStoreSizes(context.Background()) })
	st := &sizedStore{Store: memory.New(), busy: true}
	acc := accounts.NewService(acctmem.New(), true)
	s := &Server{cfg: Config{Accounts: acc}, st: st, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

	assert.Equal(t, s.sampleStoresOnce(ctx, "owner-a", now), storeSampleRetry)
	assert.Equal(t, platformSample(t, `platform_codesynapse_store_rows{db_system="nebulagraph",kind="node"}`), 0.0)

	st.busy = false
	wait := s.sampleStoresOnce(ctx, "owner-a", now.Add(storeSampleRetry))
	assert.Equal(t, wait, time.Date(2026, 10, 30, 0, 15, 0, 0, time.UTC).Sub(now.Add(storeSampleRetry)))
	assert.Equal(t, platformSample(t, `platform_codesynapse_store_rows{db_system="nebulagraph",kind="node"}`), 42.0)
	assert.Equal(t, st.calls, 2)

	// A new leader the same day does not run the count job again.
	s.sampleStoresOnce(ctx, "owner-b", now.Add(time.Hour))
	assert.Equal(t, st.calls, 2)
	// The next day it does.
	s.sampleStoresOnce(ctx, "owner-b", time.Date(2026, 10, 30, 0, 16, 0, 0, time.UTC))
	assert.Equal(t, st.calls, 3)
}
