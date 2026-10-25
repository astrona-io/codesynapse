package api

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

const jobQueuePoll = 15 * time.Second

// reportJobQueue publishes how many jobs wait for a worker, the signal an
// autoscaler adds workers on. It runs in the loops-lease holder only, so
// replicas never report it twice.
func (s *Server) reportJobQueue(ctx context.Context) {
	seen := map[string]bool{}
	t := time.NewTicker(jobQueuePoll)
	defer t.Stop()
	for {
		waiting, err := s.cfg.Accounts.St.WaitingJobs(ctx, time.Now().UTC())
		if err != nil {
			s.logger.Warn("counting waiting jobs", "error", err)
		} else {
			for kind := range waiting {
				seen[kind] = true
			}
			// A kind that drained reports 0 instead of keeping its last value.
			telemetry.Default.UpdateGauges(func() {
				for kind := range seen {
					telemetry.JobsWaiting.SetContext(ctx, float64(waiting[kind]), kind)
				}
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
