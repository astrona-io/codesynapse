package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type overviewCountingStore struct {
	store.Store
	calls   atomic.Int32
	fail    atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (s *overviewCountingStore) Stats(ctx context.Context, repo string) (model.Stats, error) {
	s.calls.Add(1)
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return model.Stats{}, ctx.Err()
		case <-s.release:
		}
	}
	if s.fail.Load() {
		return model.Stats{}, errors.New("unavailable")
	}
	return s.Store.Stats(ctx, repo)
}

func overviewFixture(t *testing.T) (*Server, *overviewCountingStore, model.Node) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := &overviewCountingStore{Store: memory.New()}
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	repos, err := st.Repos(ctx)
	require.NoError(t, err)
	s := New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, st, repos[0]
}

func TestOverviewCacheFreshnessAndAccess(t *testing.T) {
	s, st, repo := overviewFixture(t)
	ctx := context.Background()
	first, err := s.buildOverview(ctx)
	require.NoError(t, err)
	second, err := s.buildOverview(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.Totals, second.Totals)
	assert.Equal(t, st.calls.Load(), int32(1), "repeat navigation reuses graph aggregates")

	// A warm cache must not survive a loss of repository access.
	hidden := context.WithValue(ctx, scopeKey{}, scope{})
	ov, err := s.buildOverview(hidden)
	require.NoError(t, err)
	assert.Len(t, ov.Repos, 0)
	assert.Equal(t, st.calls.Load(), int32(1))

	// Reindexing the same head is a new publication too.
	repo.Meta = `{"ingest_receipt":"new-publication"}`
	_, err = s.overviewMetrics(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, st.calls.Load(), int32(2))
	s.overviews.entries[repo.Name].at = time.Now().Add(-overviewCacheTTL)
	_, err = s.overviewMetrics(ctx, repo)
	require.NoError(t, err)
	assert.Equal(t, st.calls.Load(), int32(3), "expired aggregates are recomputed")
}

func TestOverviewCacheDoesNotKeepErrors(t *testing.T) {
	s, st, repo := overviewFixture(t)
	st.fail.Store(true)
	_, err := s.overviewMetrics(context.Background(), repo)
	require.Error(t, err)
	st.fail.Store(false)
	_, err = s.overviewMetrics(context.Background(), repo)
	require.NoError(t, err)
	assert.Equal(t, st.calls.Load(), int32(2))
}

func TestOverviewCacheCoalescesRequests(t *testing.T) {
	s, st, repo := overviewFixture(t)
	st.started, st.release = make(chan struct{}, 1), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := s.overviewMetrics(context.Background(), repo)
		assert.NoError(t, err)
	})
	<-st.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.overviewMetrics(ctx, repo)
	assert.True(t, errors.Is(err, context.Canceled), "waiting requests respect cancellation")
	for range 10 {
		wg.Go(func() {
			_, err := s.overviewMetrics(context.Background(), repo)
			assert.NoError(t, err)
		})
	}
	close(st.release)
	wg.Wait()
	assert.Equal(t, st.calls.Load(), int32(1), "simultaneous page loads share the graph work")
}
