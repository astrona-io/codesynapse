package api

import (
	"context"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Store sizes (docs/PLATFORM_STATS_PLAN.md, phase 6): once a day the
// api-loops holder counts rows (and bytes, where the database reports
// them) per kind in NebulaGraph, Milvus and PostgreSQL and publishes them
// as platform_codesynapse_store_rows and _bytes. NebulaGraph counts with a
// STATS job over every partition, so the "store-sample" lease in PostgreSQL
// keeps it to one run per day across leader changes, and a publication's
// replace lease postpones it.

const (
	storeSampleLease = "store-sample"
	// storeSampleAt is the time after midnight UTC of the daily sample,
	// clear of the statistics rollup at 00:05.
	storeSampleAt = 15 * time.Minute
	// storeSampleRetry is the wait after a busy store or a failure.
	storeSampleRetry = 15 * time.Minute
)

// sampleStores runs under the api-loops lease. When the lease goes, the
// gauges go with it, so two processes never report the same figure; the
// metrics store keeps what was exported.
func (s *Server) sampleStores(ctx context.Context) {
	defer telemetry.ClearStoreSizes(context.WithoutCancel(ctx))
	owner := newOwner()
	for {
		wait := s.sampleStoresOnce(ctx, owner, time.Now())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// sampleStoresOnce takes one sample if today's has not been taken and
// answers how long to wait for the next attempt.
func (s *Server) sampleStoresOnce(ctx context.Context, owner string, now time.Time) time.Duration {
	next := accounts.StatDay(now).Add(storeSampleAt)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	// The day's lease ends a minute before the next slot. The same owner
	// takes it again to retry; another process (after a leader change)
	// waits for the next slot.
	held, err := s.cfg.Accounts.St.TakeLease(ctx, storeSampleLease, owner, next.Add(-time.Minute), now)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.logger.Warn("store sizes: taking the daily lease", "error", err)
		}
		return storeSampleRetry
	case !held:
		return next.Sub(now)
	}
	var sizes []telemetry.StoreSize
	if sz, ok := s.st.(store.Sizer); ok {
		graph, err := sz.StoreSizes(ctx)
		if errors.Is(err, store.ErrSizesBusy) {
			s.logger.Info("store sizes: a publication holds a replace lease; sampling later")
			return storeSampleRetry
		}
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("store sizes: sampling the graph store", "error", err)
			}
			return storeSampleRetry
		}
		sizes = append(sizes, graph...)
	}
	if sz, ok := s.cfg.Accounts.St.(accounts.StoreSizer); ok {
		pg, err := sz.StoreSizes(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("store sizes: sampling PostgreSQL", "error", err)
			}
			return storeSampleRetry
		}
		sizes = append(sizes, pg...)
	}
	kept := telemetry.SetStoreSizes(ctx, sizes)
	s.logger.Info("store sizes sampled", "series", kept)
	return next.Sub(now)
}
