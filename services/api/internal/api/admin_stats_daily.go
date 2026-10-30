package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// The 90d and 365d ranges of the platform statistics (docs/plans/PLATFORM_STATS_PLAN.md,
// "Daily rollup"). State figures (totals, what is active now) stay live;
// the window figures (created, revoked) are summed over the last complete
// UTC days of platform_stats_daily, read through PlatformStatsHistory.
// Figures that count distinct people or keys over the window cannot be
// summed from days: active people are the rollup's 30-day figure on the
// latest written day, and the others are listed as unavailable.

// StatsRollup describes where a 90d or 365d answer's window figures came
// from.
type StatsRollup struct {
	// Days is the length of the range; Written how many of those days the
	// rollup holds (days before it ran, or missed, are absent).
	Days    int `json:"days"`
	Written int `json:"written"`
	// Latest is the newest written day, if any.
	Latest *time.Time `json:"latest,omitempty"`
	// ActiveWindow is the window of the active-people figure: the 30 days
	// up to Latest.
	ActiveWindow string `json:"active_window"`
	// Unavailable names the figures this range cannot answer; they read 0
	// and the page shows them as not available.
	Unavailable []string `json:"unavailable"`
}

// errDailyTooLarge means a range holds more rollup rows than one read
// returns, so its sums would be short.
var errDailyTooLarge = errors.New("too many daily figures for this range; choose a shorter range or one organisation")

// dailyFigures are the summed rollup rows of one range.
type dailyFigures struct {
	rollup StatsRollup
	sum    map[[2]string]int64            // platform: metric, dimension
	last   map[[2]string]int64            // platform, on the latest written day
	org    map[string]map[[2]string]int64 // organisation ID: metric, dimension
}

func (d dailyFigures) total(metric, dim string) int {
	return int(d.sum[[2]string{metric, dim}])
}

// windowMetrics are the platform rollup metrics a long range reads.
var windowMetrics = []string{accounts.StatOrgs, accounts.StatUsers, accounts.StatActiveUsers, accounts.StatBrowserSessions, accounts.StatExecutionSessions}

// readDaily sums a long range from the rollup. orgRows also reads the
// per-organisation execution sessions (only q.OrgID's when it is set).
func (s *Server) readDaily(ctx context.Context, q accounts.StatsQuery, orgRows bool) (dailyFigures, error) {
	p, ok := principal(ctx)
	if !ok {
		return dailyFigures{}, accounts.ErrForbidden
	}
	d := dailyFigures{
		rollup: StatsRollup{Days: accounts.DailyStatRanges[q.Daily], ActiveWindow: "30d", Unavailable: []string{}},
		sum:    map[[2]string]int64{}, last: map[[2]string]int64{}, org: map[string]map[[2]string]int64{},
	}
	rows, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, windowMetrics, "", false)
	if err != nil {
		return dailyFigures{}, err
	}
	if len(rows) >= accounts.MaxPlatformStatRows {
		return dailyFigures{}, errDailyTooLarge
	}
	days := map[time.Time]bool{}
	var latest time.Time
	for _, r := range rows {
		days[r.Day] = true
		if r.Day.After(latest) {
			latest = r.Day
		}
	}
	for _, r := range rows {
		k := [2]string{r.Metric, r.Dimension}
		d.sum[k] += r.Value
		if r.Day.Equal(latest) {
			d.last[k] = r.Value
		}
	}
	d.rollup.Written = len(days)
	if !latest.IsZero() {
		d.rollup.Latest = &latest
	}
	if !orgRows {
		return d, nil
	}
	rows, err = s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, []string{accounts.StatExecutionSessions}, q.OrgID, q.OrgID == "")
	if err != nil {
		return dailyFigures{}, err
	}
	if len(rows) >= accounts.MaxPlatformStatRows {
		return dailyFigures{}, errDailyTooLarge
	}
	for _, r := range rows {
		m := d.org[r.OrgID]
		if m == nil {
			m = map[[2]string]int64{}
			d.org[r.OrgID] = m
		}
		m[[2]string{r.Metric, r.Dimension}] += r.Value
	}
	return d, nil
}

// liveState is q with an empty window: the live tables answer only the
// state figures, without scanning a long window the rollup already holds.
func liveState(q accounts.StatsQuery) accounts.StatsQuery {
	q.Since = q.Now
	return q
}

// dailyTenants fills the tenants section of a long range.
func (s *Server) dailyTenants(ctx context.Context, q accounts.StatsQuery) (accounts.TenantStats, *StatsRollup, error) {
	t, err := s.cfg.Accounts.St.TenantStats(ctx, liveState(q))
	if err != nil {
		return t, nil, err
	}
	d, err := s.readDaily(ctx, q, false)
	if err != nil {
		return t, nil, err
	}
	t.Totals.OrgsCreated = d.total(accounts.StatOrgs, "created")
	t.Totals.UsersCreated = d.total(accounts.StatUsers, "created")
	t.Totals.ActiveUsers = int(d.last[[2]string{accounts.StatActiveUsers, "30d"}])
	t.Totals.WatchAdds = 0
	for i := range t.Orgs {
		t.Orgs[i].ActiveMembers = 0
	}
	d.rollup.Unavailable = append(d.rollup.Unavailable, "watch_adds", "active_members")
	return t, &d.rollup, nil
}

// dailySessions fills the sessions section of a long range; orgRows also
// fills the organisation rows.
func (s *Server) dailySessions(ctx context.Context, q accounts.StatsQuery, orgRows bool) (accounts.SessionStats, *StatsRollup, error) {
	ss, err := s.cfg.Accounts.St.SessionStats(ctx, liveState(q))
	if err != nil {
		return ss, nil, err
	}
	d, err := s.readDaily(ctx, q, orgRows)
	if err != nil {
		return ss, nil, err
	}
	ss.Totals.BrowserCreated = d.total(accounts.StatBrowserSessions, "created")
	execution := func(c *accounts.ExecutionCounts, get func(dim string) int) {
		c.Created, c.Revoked = get("created"), get("revoked")
		c.Dashboard, c.Gateway, c.Runtime = get("created_dashboard"), get("created_gateway"), get("created_runtime")
	}
	execution(&ss.Totals.Execution, func(dim string) int { return d.total(accounts.StatExecutionSessions, dim) })
	ss.Totals.Keys.Used = 0
	for i := range ss.Orgs {
		o := &ss.Orgs[i]
		execution(&o.Execution, func(dim string) int { return int(d.org[o.OrgID][[2]string{accounts.StatExecutionSessions, dim}]) })
		o.Keys.Used = 0
	}
	d.rollup.Unavailable = append(d.rollup.Unavailable, "keys_used")
	return ss, &d.rollup, nil
}

// dailySummary fills the tiles of a long range.
func (s *Server) dailySummary(ctx context.Context, rng string, q accounts.StatsQuery) (StatsSummary, error) {
	q.OrgID = ""
	t, rollup, err := s.dailyTenants(ctx, q)
	if err != nil {
		return StatsSummary{}, err
	}
	ss, _, err := s.dailySessions(ctx, q, false)
	if err != nil {
		return StatsSummary{}, err
	}
	rollup.Unavailable = []string{}
	out := summaryOf(rng, t, ss)
	out.Rollup = rollup
	return out, nil
}

// statsError answers a failed statistics read.
func (s *Server) statsError(w http.ResponseWriter, err error) {
	if errors.Is(err, errDailyTooLarge) {
		s.problem(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.accountError(w, err)
}
