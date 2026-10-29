package api

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// statsRollupAt is how long after midnight UTC the daily platform
// statistics are written, so jobs finishing on the stroke of midnight are in.
const statsRollupAt = 5 * time.Minute

// statsRollupRetry is the wait after a failed write.
const statsRollupRetry = 10 * time.Minute

// rollupPlatformStats writes yesterday's platform statistics once a day
// (docs/PLATFORM_STATS_PLAN.md, phase 8). It runs under the api-loops lease;
// the store write is an upsert of the whole day, so a second writer after a
// lease change writes the same rows.
func (s *Server) rollupPlatformStats(ctx context.Context) {
	for {
		wait := nextStatsRollup(time.Now())
		if written, err := s.cfg.Accounts.RollupPlatformStats(ctx, s.usageStatSource); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("writing the daily platform statistics failed", "error", err)
			wait = statsRollupRetry
		} else if written {
			s.logger.Info("daily platform statistics written")
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// nextStatsRollup is the wait from now until the next write time.
func nextStatsRollup(now time.Time) time.Duration {
	next := accounts.StatDay(now).Add(statsRollupAt)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}
