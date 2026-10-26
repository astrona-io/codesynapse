package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestForEach(t *testing.T) {
	t.Run("keeps order and bounds concurrency", func(t *testing.T) {
		out := make([]int, 20)
		var running, peak atomic.Int32
		require.NoError(t, forEach(context.Background(), len(out), func(_ context.Context, i int) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			out[i] = i * i
			running.Add(-1)
			return nil
		}))
		for i, v := range out {
			assert.Equal(t, v, i*i)
		}
		assert.True(t, peak.Load() <= statsParallel)
	})
	t.Run("returns the first error", func(t *testing.T) {
		boom := errors.New("boom")
		err := forEach(context.Background(), 10, func(_ context.Context, i int) error {
			if i == 3 {
				return boom
			}
			return nil
		})
		require.ErrorIs(t, err, boom)
	})
	t.Run("a cancelled request stops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := forEach(ctx, 10, func(context.Context, int) error { return nil })
		require.ErrorIs(t, err, context.Canceled)
	})
}

type listCountingStore struct {
	store.Store
	lists atomic.Int32
}

func (s *listCountingStore) List(ctx context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	s.lists.Add(1)
	return s.Store.List(ctx, repo, f, m, limit)
}

func TestFindingRulesAreCachedUntilReindex(t *testing.T) {
	ctx := context.Background()
	st := &listCountingStore{Store: memory.New()}
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	s := New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rs := RepoSummary{Name: storetest.Repo, Head: "abc", IngestedAt: "2026-10-26T07:00:00Z"}

	first, err := s.findingRules(ctx, rs)
	require.NoError(t, err)
	again, err := s.findingRules(ctx, rs)
	require.NoError(t, err)
	assert.Equal(t, again, first)
	assert.Equal(t, st.lists.Load(), int32(1), "findings change only with an ingest")

	rs.IngestedAt = "2026-10-26T08:00:00Z"
	_, err = s.findingRules(ctx, rs)
	require.NoError(t, err)
	assert.Equal(t, st.lists.Load(), int32(2), "a new ingest reads them again")
}
