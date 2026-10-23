package api

import (
	"context"
	"time"
)

// Loops that must run in one API process at a time (scheduler, pruning,
// access projections, ingest and fix recovery) run while this process holds
// the "api-loops" lease in the accounts store, so several API replicas are
// safe (docs/SAAS.md, 5e). Without accounts there is one process.

const (
	loopsLease      = "api-loops"
	loopsLeaseTTL   = time.Minute
	loopsLeaseRenew = 20 * time.Second
)

// singleton runs loops while this process leads. Each loop gets a context
// that ends when the lease is lost or cannot be renewed, and starts again
// when the lease comes back.
func (s *Server) singleton(ctx context.Context, loops ...func(context.Context)) {
	if s.cfg.Accounts == nil {
		for _, loop := range loops {
			go loop(ctx)
		}
		return
	}
	owner := newOwner()
	go func() {
		t := time.NewTicker(loopsLeaseRenew)
		defer t.Stop()
		var stop context.CancelFunc
		for {
			now := time.Now()
			held, err := s.cfg.Accounts.St.TakeLease(ctx, loopsLease, owner, now.Add(loopsLeaseTTL), now)
			if err != nil && ctx.Err() == nil {
				// Without a renewal another process may take over: stop.
				s.logger.Warn("renewing the loops lease", "error", err)
				held = false
			}
			switch {
			case held && stop == nil:
				var leadCtx context.Context
				leadCtx, stop = context.WithCancel(ctx)
				for _, loop := range loops {
					go loop(leadCtx)
				}
				s.logger.Info("this API process runs the scheduler and maintenance loops")
			case !held && stop != nil:
				stop()
				stop = nil
				s.logger.Info("another API process runs the scheduler and maintenance loops")
			}
			select {
			case <-ctx.Done():
				if stop != nil {
					stop()
				}
				return
			case <-t.C:
			}
		}
	}()
}
