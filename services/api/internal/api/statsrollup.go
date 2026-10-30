package api

import (
	"context"
	"math"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// statsRollupAt is how long after midnight UTC the daily platform
// statistics are written, so jobs finishing on the stroke of midnight are in.
const statsRollupAt = 5 * time.Minute

// statsRollupRetry is the wait after a failed write.
const statsRollupRetry = 10 * time.Minute

// rollupPlatformStats writes yesterday's platform statistics once a day
// (docs/plans/PLATFORM_STATS_PLAN.md, phase 8). It runs under the api-loops lease;
// the store write is an upsert of the whole day, so a second writer after a
// lease change writes the same rows.
func (s *Server) rollupPlatformStats(ctx context.Context) {
	for {
		wait := nextStatsRollup(time.Now())
		if written, err := s.cfg.Accounts.RollupPlatformStats(ctx, s.usageStatSource, s.gatewayStatSource, s.securityStatSource, s.auditStatSource); err != nil {
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

// gatewayRollupQueries are the fixed queries of a day's gateway traffic per
// organisation, evaluated at the end of the day. They run across tenants
// (the rollup is platform work) and are split by tenant_id here.
var gatewayRollupQueries = struct{ requests, fallthroughs, latencySum, latencyCount, queueSum, queueCount string }{
	requests:     `sum by (tenant_id, route, class) (increase(gateway_requests_total[1d]))`,
	fallthroughs: `sum by (tenant_id, reason) (increase(gateway_fallthrough_total[1d]))`,
	latencySum:   `sum by (tenant_id, route) (increase(gateway_request_duration_seconds_sum[1d]))`,
	latencyCount: `sum by (tenant_id, route) (increase(gateway_request_duration_seconds_count[1d]))`,
	queueSum:     `sum by (tenant_id) (increase(gateway_queue_wait_seconds_sum[1d]))`,
	queueCount:   `sum by (tenant_id) (increase(gateway_queue_wait_seconds_count[1d]))`,
}

// gatewayStatSource adds a day's gateway traffic to the daily rollup, so
// timeouts and response times outlive the metrics store's retention. A
// metrics store that does not answer leaves the day's gateway rows at zero
// with measured 0 instead of holding back the accounts figures.
func (s *Server) gatewayStatSource(ctx context.Context, day time.Time, set *accounts.PlatformStatSet) error {
	if s.cfg.Metrics == nil {
		return nil
	}
	end := day.AddDate(0, 0, 1)
	tmp := accounts.NewPlatformStatSet(day)
	add := func(tenant, dim string, v float64) {
		n := int64(math.Round(v))
		if n <= 0 {
			return
		}
		if accounts.ValidPlatformStat(accounts.StatGateway, dim, tenant) && tenant != "" {
			tmp.Both(tenant, accounts.StatGateway, dim, n)
			return
		}
		tmp.Total(accounts.StatGateway, dim, n) // no organisation, or one the rollup cannot hold
	}
	q := gatewayRollupQueries
	for _, st := range []struct {
		expr string
		add  func(map[string]string, float64)
	}{
		{q.requests, func(l map[string]string, v float64) {
			t := l["tenant_id"]
			add(t, "requests", v)
			switch l["route"] {
			case "local", "cloud":
				add(t, l["route"], v)
			default:
				add(t, "rejected", v)
			}
			if l["class"] == "4xx" || l["class"] == "5xx" {
				add(t, "failed", v)
			}
		}},
		{q.fallthroughs, func(l map[string]string, v float64) {
			reason := telemetry.GatewayReason(l["reason"])
			add(l["tenant_id"], "fallthroughs", v)
			if telemetry.GatewayTimeout(reason) {
				add(l["tenant_id"], reason, v)
			}
		}},
		{q.latencySum, func(l map[string]string, v float64) {
			if r := l["route"]; r == "local" || r == "cloud" {
				add(l["tenant_id"], r+"_ms", v*1000)
			}
		}},
		{q.latencyCount, func(l map[string]string, v float64) {
			if r := l["route"]; r == "local" || r == "cloud" {
				add(l["tenant_id"], r+"_n", v)
			}
		}},
		{q.queueSum, func(l map[string]string, v float64) { add(l["tenant_id"], "queue_ms", v*1000) }},
		{q.queueCount, func(l map[string]string, v float64) { add(l["tenant_id"], "queue_n", v) }},
	} {
		res, err := s.cfg.Metrics.QueryRange(ctx, st.expr, end, end, 24*time.Hour, nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("the daily gateway statistics are written without the metrics store", "error", err)
			return nil
		}
		for _, ts := range res {
			if len(ts.Points) == 0 {
				continue
			}
			if v := ts.Points[len(ts.Points)-1].V; !math.IsNaN(v) && !math.IsInf(v, 0) {
				st.add(ts.Labels, v)
			}
		}
	}
	for _, r := range tmp.Rows() {
		if r.Metric == accounts.StatGateway && r.Value > 0 {
			set.Add(r)
		}
	}
	set.Total(accounts.StatGateway, "measured", 1)
	return nil
}
