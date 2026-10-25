package api

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

const jobQueuePoll = 15 * time.Second

// reportJobQueue publishes how many jobs wait for a worker and how many are
// active, the signals an autoscaler adds workers on. It runs in the loops-lease holder only, so
// replicas never report it twice.
func (s *Server) reportJobQueue(ctx context.Context) {
	seen := map[string]bool{}
	t := time.NewTicker(jobQueuePoll)
	defer t.Stop()
	for {
		q, err := s.cfg.Accounts.St.JobQueue(ctx, time.Now().UTC())
		if err != nil {
			s.logger.Warn("counting waiting jobs", "error", err)
		} else {
			for kind := range q.Active {
				seen[kind] = true
			}
			// A kind that drained reports 0 instead of keeping its last value.
			telemetry.Default.UpdateGauges(func() {
				for kind := range seen {
					telemetry.JobsWaiting.SetContext(ctx, float64(q.Waiting[kind]), kind)
					telemetry.JobsActive.SetContext(ctx, float64(q.Active[kind]), kind)
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
