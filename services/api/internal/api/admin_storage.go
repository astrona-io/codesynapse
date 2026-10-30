package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Storage and alerts (docs/plans/PLATFORM_STATS_PLAN.md, phase 6). Storage reads
// the daily store sample (storesample.go) back from VictoriaMetrics, so any
// API replica answers the same figures; alerts read the internal vmalert.

// The store queries are constants. A sample is taken once a day, so the
// last one within two days is the current size; max by (db_system, kind)
// takes one value whichever replica exported it (gauges are never summed
// across replicas).
const (
	storeRowsQuery  = `max by (db_system, kind) (last_over_time(platform_codesynapse_store_rows[2d]))`
	storeBytesQuery = `max by (db_system, kind) (last_over_time(platform_codesynapse_store_bytes[2d]))`
	storeTimeQuery  = `max(tlast_over_time(platform_codesynapse_store_rows[2d]))`
)

// StoreSizeRow is one kind in one store: the latest sample and its change
// over the range. Nil means the store does not report that figure.
type StoreSizeRow struct {
	System      string   `json:"db_system"`
	Kind        string   `json:"kind"`
	Rows        *float64 `json:"rows"`
	Bytes       *float64 `json:"bytes"`
	RowsChange  *float64 `json:"rows_change"`
	BytesChange *float64 `json:"bytes_change"`
}

// StatsStorage answers GET /api/v1/admin/stats/storage.
type StatsStorage struct {
	Range string `json:"range"`
	// Available is false without a metrics store or before the first
	// sample; Reason says which.
	Available bool           `json:"available"`
	Reason    string         `json:"reason,omitempty"`
	SampledAt *time.Time     `json:"sampled_at"`
	Stores    []StoreSizeRow `json:"stores"`
	// Rollup is set for 90d and 365d, which read the daily rollup: the
	// sizes of the latest written day and their change since the first.
	Rollup *StatsRollup `json:"rollup,omitempty"`
}

func (s *Server) handleStatsStorage(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "storage")
	req.OrgID = "" // sizes are the platform's
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	out, err := s.statsStorage(r.Context(), req.Range, q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// storeStep keeps a range query to about 60 points, never under an hour:
// the figures change once a day.
func storeStep(since, now time.Time) time.Duration {
	return max(now.Sub(since)/60, time.Hour).Truncate(time.Hour)
}

func (s *Server) statsStorage(ctx context.Context, rng string, q accounts.StatsQuery) (StatsStorage, error) {
	out := StatsStorage{Range: rng, Stores: []StoreSizeRow{}}
	if q.Daily != "" {
		return s.dailyStorage(ctx, out, q)
	}
	if s.cfg.Metrics == nil {
		out.Reason = "no metrics store"
		return out, nil
	}
	since, now := q.Since, q.Now
	if since.IsZero() {
		since = now.Add(-24 * time.Hour)
	}
	step := storeStep(since, now)
	rows, err := s.cfg.Metrics.QueryRange(ctx, storeRowsQuery, since, now, step, nil)
	if err != nil {
		return out, err
	}
	bytes, err := s.cfg.Metrics.QueryRange(ctx, storeBytesQuery, since, now, step, nil)
	if err != nil {
		return out, err
	}
	at, err := s.cfg.Metrics.QueryRange(ctx, storeTimeQuery, now, now, step, nil)
	if err != nil {
		return out, err
	}
	out.Stores = storeRows(rows, bytes)
	if len(out.Stores) == 0 {
		out.Reason = "no sample yet"
		return out, nil
	}
	out.Available = true
	if len(at) > 0 && len(at[0].Points) > 0 {
		t := time.Unix(int64(at[0].Points[len(at[0].Points)-1].V), 0).UTC()
		out.SampledAt = &t
	}
	return out, nil
}

// storeRows joins the rows and bytes series by store and kind. Only kinds
// the stores registered (telemetry.StoreKindAllowed) are shown, whatever
// the metrics store holds.
func storeRows(rows, bytes []telemetry.Series) []StoreSizeRow {
	type key struct{ system, kind string }
	by := map[key]*StoreSizeRow{}
	get := func(sr telemetry.Series) *StoreSizeRow {
		k := key{sr.Labels["db_system"], sr.Labels["kind"]}
		if !telemetry.StoreKindAllowed(k.system, k.kind) || len(sr.Points) == 0 {
			return nil
		}
		if by[k] == nil {
			by[k] = &StoreSizeRow{System: k.system, Kind: k.kind}
		}
		return by[k]
	}
	latest := func(sr telemetry.Series) (*float64, *float64) {
		first, last := sr.Points[0].V, sr.Points[len(sr.Points)-1].V
		change := last - first
		return &last, &change
	}
	for _, sr := range rows {
		if row := get(sr); row != nil {
			row.Rows, row.RowsChange = latest(sr)
		}
	}
	for _, sr := range bytes {
		if row := get(sr); row != nil {
			row.Bytes, row.BytesChange = latest(sr)
		}
	}
	out := make([]StoreSizeRow, 0, len(by))
	for _, row := range by {
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b StoreSizeRow) int {
		return cmp.Or(cmp.Compare(a.System, b.System), cmp.Compare(a.Kind, b.Kind))
	})
	return out
}

// dailyStorage answers 90d and 365d from the daily rollup: per store and
// kind the size on the latest day that has one, and the change since the
// first such day in the range. Kinds no longer registered are left out.
func (s *Server) dailyStorage(ctx context.Context, out StatsStorage, q accounts.StatsQuery) (StatsStorage, error) {
	p, ok := principal(ctx)
	if !ok {
		return out, accounts.ErrForbidden
	}
	roll := StatsRollup{Days: accounts.DailyStatRanges[q.Daily], Unavailable: []string{}}
	out.Rollup = &roll
	hist, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, []string{accounts.StatStoreRows, accounts.StatStoreBytes}, "", false)
	if err != nil {
		return out, err
	}
	if len(hist) >= accounts.MaxPlatformStatRows {
		return out, errDailyTooLarge
	}
	type point struct {
		first, last       int64
		firstDay, lastDay time.Time
	}
	series := map[[2]string]*point{} // metric, dimension
	days := map[time.Time]bool{}
	for _, r := range hist {
		if r.OrgID != "" {
			continue
		}
		days[r.Day] = true
		k := [2]string{r.Metric, r.Dimension}
		pt := series[k]
		if pt == nil {
			series[k] = &point{first: r.Value, last: r.Value, firstDay: r.Day, lastDay: r.Day}
			continue
		}
		if r.Day.Before(pt.firstDay) {
			pt.first, pt.firstDay = r.Value, r.Day
		}
		if r.Day.After(pt.lastDay) {
			pt.last, pt.lastDay = r.Value, r.Day
		}
	}
	roll.Written = len(days)
	for d := range days {
		if roll.Latest == nil || d.After(*roll.Latest) {
			day := d
			roll.Latest = &day
		}
	}
	type key struct{ system, kind string }
	by := map[key]*StoreSizeRow{}
	for k, pt := range series {
		system, kind, _ := strings.Cut(k[1], ".")
		if !telemetry.StoreKindAllowed(system, kind) {
			continue
		}
		row := by[key{system, kind}]
		if row == nil {
			row = &StoreSizeRow{System: system, Kind: kind}
			by[key{system, kind}] = row
		}
		last, change := float64(pt.last), float64(pt.last-pt.first)
		if k[0] == accounts.StatStoreRows {
			row.Rows, row.RowsChange = &last, &change
		} else {
			row.Bytes, row.BytesChange = &last, &change
		}
	}
	for _, row := range by {
		out.Stores = append(out.Stores, *row)
	}
	slices.SortFunc(out.Stores, func(a, b StoreSizeRow) int {
		return cmp.Or(cmp.Compare(a.System, b.System), cmp.Compare(a.Kind, b.Kind))
	})
	if len(out.Stores) == 0 {
		out.Reason = "no sample in the daily rollup yet"
		return out, nil
	}
	out.Available = true
	if roll.Latest != nil {
		at := *roll.Latest
		out.SampledAt = &at
	}
	return out, nil
}

// StatsAlerts answers GET /api/v1/admin/stats/alerts.
type StatsAlerts struct {
	// Configured is false without CODESYNAPSE_ALERTS_URL; Reachable is
	// false when vmalert did not answer.
	Configured bool                  `json:"configured"`
	Reachable  bool                  `json:"reachable"`
	Firing     int                   `json:"firing"`
	Pending    int                   `json:"pending"`
	Alerts     []telemetry.Alert     `json:"alerts"`
	Rules      []telemetry.AlertRule `json:"rules"`
}

func (s *Server) handleStatsAlerts(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "alerts")
	req.OrgID, req.Range = "", ""
	if _, ok := s.platformView(w, r, req); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.statsAlerts(r.Context()))
}

// alertsTimeout bounds one look at vmalert, so a slow evaluator never
// holds the Statistics page.
const alertsTimeout = 5 * time.Second

func (s *Server) statsAlerts(ctx context.Context) StatsAlerts {
	out := StatsAlerts{Alerts: []telemetry.Alert{}, Rules: []telemetry.AlertRule{}}
	if s.cfg.Alerts == nil {
		return out
	}
	out.Configured = true
	ctx, cancel := context.WithTimeout(ctx, alertsTimeout)
	defer cancel()
	alerts, err := s.cfg.Alerts.Alerts(ctx)
	if err != nil {
		s.logger.Warn("statistics: reading alerts", "error", err)
		return out
	}
	rules, err := s.cfg.Alerts.Rules(ctx)
	if err != nil {
		s.logger.Warn("statistics: reading alert rules", "error", err)
		return out
	}
	out.Reachable, out.Alerts, out.Rules = true, alerts, rules
	for _, a := range alerts {
		switch a.State {
		case "firing":
			out.Firing++
		case "pending":
			out.Pending++
		}
	}
	return out
}

// alertsFiring fills the summary tile: nil when alerts are not configured
// or vmalert did not answer.
func (s *Server) alertsFiring(ctx context.Context) *int {
	if s.cfg.Alerts == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, alertsTimeout)
	defer cancel()
	alerts, err := s.cfg.Alerts.Alerts(ctx)
	if err != nil {
		return nil
	}
	n := 0
	for _, a := range alerts {
		if a.State == "firing" {
			n++
		}
	}
	return &n
}
