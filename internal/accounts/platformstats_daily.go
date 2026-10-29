package accounts

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Daily platform statistics (docs/PLATFORM_STATS_PLAN.md, "Daily rollup").
// The process holding the api-loops lease writes yesterday's counts shortly
// after midnight UTC, and the 90-day and 365-day statistics ranges read
// them. A row holds a day, a metric and a dimension from the fixed lists
// below, an opaque organisation ID (empty for platform totals) and a count:
// never a name, an e-mail address, a repository or the ID of a user, key or
// session. Deleting an organisation keeps its rows.

// Metrics of the daily rollup.
const (
	StatOrgs              = "orgs"
	StatUsers             = "users"
	StatActiveUsers       = "active_users"
	StatMemberships       = "memberships"
	StatSeats             = "seats"
	StatRepoEntries       = "repo_entries"
	StatGraphs            = "graphs"
	StatJobsFinished      = "jobs_finished"
	StatBrowserSessions   = "browser_sessions"
	StatExecutionSessions = "execution_sessions"
	StatAPIKeys           = "api_keys"
)

// Limits and retention of the daily rollup.
const (
	// PlatformStatsRetentionDays is how long rows are kept (decided: 400
	// days, the default audit retention, independent of it).
	PlatformStatsRetentionDays = 400
	// MaxPlatformStatsPerDay bounds the rows one day may hold.
	MaxPlatformStatsPerDay = 200_000
	// MaxPlatformStatRows bounds one read.
	MaxPlatformStatRows = 50_000
)

// StatJobKinds are the job kinds the rollup names; any other kind counts as
// "other". They mirror the durable job kinds of the API (jobs_durable.go).
var StatJobKinds = []string{"sync", "clone", "switch", "ai", "ai-auto", "fix", "other"}

// statJobOutcomes are the finished job states.
var statJobOutcomes = []string{"succeeded", "failed"}

// statMetric describes one metric: its dimensions and whether it also has
// rows per organisation.
type statMetric struct {
	dims   []string
	perOrg bool
}

var statMetrics = map[string]statMetric{
	// Organisations at the end of the day, created that day, with no
	// member other than root, with single sign-on.
	StatOrgs: {dims: []string{"total", "created", "no_members", "sso"}},
	// Distinct accounts other than root.
	StatUsers: {dims: []string{"total", "created", "suspended", "platform_admins", "guest_only", "multi_org"}},
	// Distinct accounts other than root with a sign-in, a session, key use
	// or a new execution session in the window ending with the day.
	StatActiveUsers: {dims: []string{"1d", "7d", "30d"}},
	// Memberships of accounts other than root, by role. The platform total
	// is the sum over organisations, so a person counts once per
	// organisation.
	StatMemberships: {dims: []string{"total", "owner", "admin", "member", "viewer", "guest"}, perOrg: true},
	// Active non-guest members; the platform row counts distinct people.
	StatSeats: {dims: []string{"total"}, perOrg: true},
	// Repository entries by space, visibility and lost access.
	StatRepoEntries: {dims: []string{"total", "organisation", "personal", "public", "private", "local", "lost"}, perOrg: true},
	// Distinct graphs: a public graph linked by several organisations
	// counts once.
	StatGraphs: {dims: []string{"total"}},
	// Jobs that finished that day, by kind and outcome ("sync.failed").
	StatJobsFinished: {dims: jobDims(), perOrg: true},
	// Browser sessions active at the end of the day and created that day.
	StatBrowserSessions: {dims: []string{"active", "created"}},
	// Execution sessions active at the end of the day, created that day
	// (dashboard bindings, gateway keys and agent runtimes) and revoked.
	StatExecutionSessions: {dims: []string{"active", "created", "created_dashboard", "created_gateway", "created_runtime", "revoked"}, perOrg: true},
	// API keys active at the end of the day, used that day, and active
	// ones expiring within 14 days.
	StatAPIKeys: {dims: []string{"active", "used", "expiring_14d"}, perOrg: true},
}

func jobDims() []string {
	out := []string{}
	for _, k := range StatJobKinds {
		for _, o := range statJobOutcomes {
			out = append(out, k+"."+o)
		}
	}
	return out
}

// StatJobDimension is the jobs_finished dimension of a job kind and final
// state; ok is false for a state that is not a finished one.
func StatJobDimension(kind, state string) (string, bool) {
	if !slices.Contains(statJobOutcomes, state) {
		return "", false
	}
	if !slices.Contains(StatJobKinds, kind) {
		kind = "other"
	}
	return kind + "." + state, true
}

// PlatformStatMetrics lists the metrics in a stable order.
func PlatformStatMetrics() []string {
	out := make([]string, 0, len(statMetrics))
	for m := range statMetrics {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// PlatformStatDimensions lists a metric's dimensions (nil for an unknown one).
func PlatformStatDimensions(metric string) []string {
	return slices.Clone(statMetrics[metric].dims)
}

// PlatformStat is one row of the daily rollup.
type PlatformStat struct {
	Day       time.Time `json:"day"`
	Metric    string    `json:"metric"`
	Dimension string    `json:"dimension"`
	// OrgID is empty for platform totals.
	OrgID string `json:"org_id,omitempty"`
	Value int64  `json:"value"`
}

// PlatformStatQuery selects rows of the rollup.
type PlatformStatQuery struct {
	// From and To are days (UTC midnight): From inclusive, To exclusive, at
	// most PlatformStatsRetentionDays apart.
	From, To time.Time
	// Metrics limits the rows to these metrics; empty means all.
	Metrics []string
	// OrgID selects one organisation's rows. Empty with PerOrg false
	// selects the platform totals; PerOrg selects every organisation's.
	OrgID  string
	PerOrg bool
	// Limit is capped at MaxPlatformStatRows; zero means that cap.
	Limit int
}

// StatDay is the UTC day holding t.
func StatDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func isStatDay(t time.Time) bool {
	return !t.IsZero() && t.Equal(StatDay(t)) && t.Location() == time.UTC
}

// ValidPlatformStat reports whether a metric, dimension and organisation
// form a row the rollup may hold.
func ValidPlatformStat(metric, dimension, orgID string) bool {
	m, ok := statMetrics[metric]
	if !ok || !slices.Contains(m.dims, dimension) {
		return false
	}
	return orgID == "" || (m.perOrg && policyText(orgID, 128))
}

// PreparePlatformStats validates the rows of one day and returns them
// sorted. Stores call it before writing, so nothing outside the lists is
// stored.
func PreparePlatformStats(day time.Time, rows []PlatformStat) ([]PlatformStat, error) {
	if !isStatDay(day) || len(rows) > MaxPlatformStatsPerDay {
		return nil, ErrInvalidInput
	}
	out := make([]PlatformStat, 0, len(rows))
	seen := map[[3]string]bool{}
	for _, r := range rows {
		if !r.Day.Equal(day) || r.Value < 0 || !ValidPlatformStat(r.Metric, r.Dimension, r.OrgID) {
			return nil, fmt.Errorf("%w: platform statistic %q/%q", ErrInvalidInput, r.Metric, r.Dimension)
		}
		k := [3]string{r.Metric, r.Dimension, r.OrgID}
		if seen[k] {
			return nil, fmt.Errorf("%w: duplicate platform statistic %q/%q", ErrInvalidInput, r.Metric, r.Dimension)
		}
		seen[k] = true
		r.Day = day
		out = append(out, r)
	}
	SortPlatformStats(out)
	return out, nil
}

// PreparePlatformStatQuery validates q and applies the limits.
func PreparePlatformStatQuery(q PlatformStatQuery) (PlatformStatQuery, error) {
	if !isStatDay(q.From) || !isStatDay(q.To) || !q.To.After(q.From) || q.To.Sub(q.From) > PlatformStatsRetentionDays*24*time.Hour {
		return PlatformStatQuery{}, ErrInvalidInput
	}
	if q.OrgID != "" && (q.PerOrg || !policyText(q.OrgID, 128)) {
		return PlatformStatQuery{}, ErrInvalidInput
	}
	for _, m := range q.Metrics {
		if _, ok := statMetrics[m]; !ok {
			return PlatformStatQuery{}, ErrInvalidInput
		}
	}
	q.Metrics = slices.Compact(slices.Sorted(slices.Values(q.Metrics)))
	if q.Limit <= 0 || q.Limit > MaxPlatformStatRows {
		q.Limit = MaxPlatformStatRows
	}
	return q, nil
}

// MatchPlatformStat reports whether r is selected by a prepared query.
func MatchPlatformStat(q PlatformStatQuery, r PlatformStat) bool {
	if r.Day.Before(q.From) || !r.Day.Before(q.To) || (len(q.Metrics) > 0 && !slices.Contains(q.Metrics, r.Metric)) {
		return false
	}
	switch {
	case q.PerOrg:
		return r.OrgID != ""
	default:
		return r.OrgID == q.OrgID
	}
}

// SortPlatformStats orders rows by day, metric, dimension and organisation,
// the order every read returns.
func SortPlatformStats(rows []PlatformStat) {
	slices.SortFunc(rows, func(a, b PlatformStat) int {
		return cmp.Or(a.Day.Compare(b.Day), strings.Compare(a.Metric, b.Metric), strings.Compare(a.Dimension, b.Dimension), strings.Compare(a.OrgID, b.OrgID))
	})
}

// PlatformStatSet collects one day's counts. Stores fill it from their
// sources; Rows writes every platform row (zeros included, so a written day
// is recognisable and complete) and the non-zero organisation rows.
type PlatformStatSet struct {
	day      time.Time
	platform map[[2]string]int64
	orgs     map[[3]string]int64
}

// NewPlatformStatSet starts the counts of a day.
func NewPlatformStatSet(day time.Time) *PlatformStatSet {
	return &PlatformStatSet{day: day, platform: map[[2]string]int64{}, orgs: map[[3]string]int64{}}
}

// Total adds n to a platform total.
func (s *PlatformStatSet) Total(metric, dimension string, n int64) {
	s.platform[[2]string{metric, dimension}] += n
}

// Org adds n to an organisation's row only.
func (s *PlatformStatSet) Org(orgID, metric, dimension string, n int64) {
	if orgID == "" {
		return
	}
	s.orgs[[3]string{metric, dimension, orgID}] += n
}

// Both adds n to an organisation's row and to the platform total.
func (s *PlatformStatSet) Both(orgID, metric, dimension string, n int64) {
	s.Org(orgID, metric, dimension, n)
	s.Total(metric, dimension, n)
}

// Rows returns the rows, sorted.
func (s *PlatformStatSet) Rows() []PlatformStat {
	out := []PlatformStat{}
	for _, m := range PlatformStatMetrics() {
		for _, d := range statMetrics[m].dims {
			out = append(out, PlatformStat{Day: s.day, Metric: m, Dimension: d, Value: s.platform[[2]string{m, d}]})
		}
	}
	for k, v := range s.orgs {
		if v != 0 {
			out = append(out, PlatformStat{Day: s.day, Metric: k[0], Dimension: k[1], OrgID: k[2], Value: v})
		}
	}
	SortPlatformStats(out)
	return out
}

// StatActiveWindows are the active-user windows in days, by dimension.
var StatActiveWindows = map[string]int{"1d": 1, "7d": 7, "30d": 30}

// RollupPlatformStats writes the rows of the day before now's UTC day unless
// that day is already written, then prunes rows past the retention. written
// reports whether this call wrote the day. A retry or a second writer finds
// the day written and changes nothing; writing the same rows twice leaves
// the same rows.
func (s *Service) RollupPlatformStats(ctx context.Context) (written bool, err error) {
	now := s.now()
	day := StatDay(now).AddDate(0, 0, -1)
	done, err := s.St.HasPlatformStats(ctx, day)
	if err != nil {
		return false, fmt.Errorf("checking the platform statistics of %s: %w", day.Format(time.DateOnly), err)
	}
	if !done {
		rows, err := s.St.CountPlatformStats(ctx, day)
		if err != nil {
			return false, fmt.Errorf("counting the platform statistics of %s: %w", day.Format(time.DateOnly), err)
		}
		if err := s.St.PutPlatformStats(ctx, day, rows); err != nil {
			return false, fmt.Errorf("writing the platform statistics of %s: %w", day.Format(time.DateOnly), err)
		}
		written = true
	}
	if _, err := s.St.PrunePlatformStats(ctx, StatDay(now).AddDate(0, 0, -PlatformStatsRetentionDays)); err != nil {
		return written, fmt.Errorf("pruning platform statistics: %w", err)
	}
	return written, nil
}

// DailyStatRanges are the statistics ranges the rollup answers, in days.
// Shorter ranges (1h to 30d) come from VictoriaMetrics.
var DailyStatRanges = map[string]int{"90d": 90, "365d": 365}

// DailyStatRange turns a range name into the days it covers: the last n
// complete UTC days before now. ok is false for a range the rollup does not
// answer.
func DailyStatRange(name string, now time.Time) (from, to time.Time, ok bool) {
	n, ok := DailyStatRanges[name]
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	to = StatDay(now)
	return to.AddDate(0, 0, -n), to, true
}

// PlatformStatsHistory reads the daily rollup for a 90d or 365d range for a
// platform administrator in a signed-in session. It does not audit: the
// statistics handler calls PlatformStats first, which writes the view event.
// orgID selects one organisation's rows (a deleted organisation's included),
// perOrg every organisation's, neither the platform totals.
func (s *Service) PlatformStatsHistory(ctx context.Context, p Principal, rangeName string, metrics []string, orgID string, perOrg bool) ([]PlatformStat, error) {
	if err := s.requirePlatform(ctx, p); err != nil {
		return nil, err
	}
	from, to, ok := DailyStatRange(rangeName, s.now())
	if !ok {
		return nil, ErrInvalidInput
	}
	return s.St.ListPlatformStats(ctx, PlatformStatQuery{From: from, To: to, Metrics: metrics, OrgID: orgID, PerOrg: perOrg})
}
