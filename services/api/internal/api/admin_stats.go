package api

import (
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Platform statistics (docs/PLATFORM_STATS_PLAN.md, phase 1): aggregates
// across every organisation for platform administrators, from PostgreSQL.
// Every view goes through accounts.Service.PlatformStats, which allows only
// a platform administrator's signed-in session and writes the audit entry
// before any figure is read.

// platformView authorises and audits one platform view. It writes the
// error itself; false means stop.
func (s *Server) platformView(w http.ResponseWriter, r *http.Request, req accounts.StatsRequest) (accounts.StatsQuery, bool) {
	p, ok := s.caller(w, r)
	if !ok {
		return accounts.StatsQuery{}, false
	}
	q, err := s.cfg.Accounts.PlatformStats(r.Context(), p, req)
	if err != nil {
		s.accountError(w, err)
		return accounts.StatsQuery{}, false
	}
	return q, true
}

// statsRequest reads the range and organisation filter shared by the
// statistics routes. The service checks both against its allowlist and
// PostgreSQL.
func statsRequest(r *http.Request, section string) accounts.StatsRequest {
	q := r.URL.Query()
	return accounts.StatsRequest{Section: section, Range: cmp.Or(q.Get("range"), "24h"), OrgID: q.Get("org")}
}

// StatsSummary is the tile row of the Statistics page. Tiles that later
// phases fill (graphs, jobs, cost, alerts) are null until then.
type StatsSummary struct {
	Range            string `json:"range"`
	Orgs             int    `json:"orgs"`
	OrgsCreated      int    `json:"orgs_created"`
	Users            int    `json:"users"`
	UsersCreated     int    `json:"users_created"`
	ActiveUsers      int    `json:"active_users"`
	Memberships      int    `json:"memberships"`
	Seats            int    `json:"seats"`
	SeatedPeople     int    `json:"seated_people"`
	ExecutionActive  int    `json:"execution_active"`
	ExecutionCreated int    `json:"execution_created"`
	KeysActive       int    `json:"keys_active"`
	BrowserActive    int    `json:"browser_active"`

	Graphs       *int     `json:"graphs"`
	JobsWaiting  *int     `json:"jobs_waiting"`
	JobsFailed   *int     `json:"jobs_failed"`
	Cost         *float64 `json:"cost"`
	AlertsFiring *int     `json:"alerts_firing"`

	// Rollup is set for 90d and 365d, whose window figures come from the
	// daily rollup.
	Rollup *StatsRollup `json:"rollup,omitempty"`
}

func (s *Server) statsSummary(ctx context.Context, rng string, q accounts.StatsQuery) (StatsSummary, error) {
	q.OrgID = "" // the tiles are the platform's
	if q.Daily != "" {
		return s.dailySummary(ctx, rng, q)
	}
	t, err := s.cfg.Accounts.St.TenantStats(ctx, q)
	if err != nil {
		return StatsSummary{}, err
	}
	ss, err := s.cfg.Accounts.St.SessionStats(ctx, q)
	if err != nil {
		return StatsSummary{}, err
	}
	return summaryOf(rng, t, ss), nil
}

func summaryOf(rng string, t accounts.TenantStats, ss accounts.SessionStats) StatsSummary {
	return StatsSummary{
		Range: rng, Orgs: t.Totals.Orgs, OrgsCreated: t.Totals.OrgsCreated,
		Users: t.Totals.Users, UsersCreated: t.Totals.UsersCreated, ActiveUsers: t.Totals.ActiveUsers,
		Memberships: t.Totals.Memberships, Seats: t.Totals.Seats, SeatedPeople: t.Totals.SeatedPeople,
		ExecutionActive: ss.Totals.Execution.Active, ExecutionCreated: ss.Totals.Execution.Created,
		KeysActive: ss.Totals.Keys.Active, BrowserActive: ss.Totals.BrowserActive,
	}
}

// tenantStats and sessionStats read a section: the live tables for the
// short ranges, the live state plus the daily rollup for 90d and 365d.
func (s *Server) tenantStats(ctx context.Context, q accounts.StatsQuery) (accounts.TenantStats, *StatsRollup, error) {
	if q.Daily != "" {
		return s.dailyTenants(ctx, q)
	}
	t, err := s.cfg.Accounts.St.TenantStats(ctx, q)
	return t, nil, err
}

func (s *Server) sessionStats(ctx context.Context, q accounts.StatsQuery) (accounts.SessionStats, *StatsRollup, error) {
	if q.Daily != "" {
		return s.dailySessions(ctx, q, true)
	}
	ss, err := s.cfg.Accounts.St.SessionStats(ctx, q)
	return ss, nil, err
}

func (s *Server) handleStatsSummary(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "summary")
	req.OrgID = ""
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	out, err := s.statsSummary(r.Context(), req.Range, q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// tenantSorts and sessionSorts are the columns a table sorts by; names
// sort ascending, counts descending, ties by name.
var tenantSorts = map[string]func(accounts.OrgTenantRow) int{
	"members": func(o accounts.OrgTenantRow) int { return o.Members },
	"seats":   func(o accounts.OrgTenantRow) int { return o.Seats },
	"active":  func(o accounts.OrgTenantRow) int { return o.ActiveMembers },
	"entries": func(o accounts.OrgTenantRow) int { return o.Entries },
}

var sessionSorts = map[string]func(accounts.OrgSessionRow) int{
	"execution": func(o accounts.OrgSessionRow) int { return o.Execution.Active },
	"keys":      func(o accounts.OrgSessionRow) int { return o.Keys.Active },
}

// sortRows orders rows by an allowlisted count, keeping the store's name
// order for ties. Unknown or empty sorts keep the name order.
func sortRows[T any](rows []T, by func(T) int) {
	if by != nil {
		slices.SortStableFunc(rows, func(a, b T) int { return cmp.Compare(by(b), by(a)) })
	}
}

// StatsTable is one page of a statistics table with the platform totals.
type StatsTable[Totals, Row any] struct {
	Range  string `json:"range"`
	Totals Totals `json:"totals"`
	Rows   []Row  `json:"rows"`
	Page   int    `json:"page"`
	Pages  int    `json:"pages"`
	Total  int    `json:"total"`
	// Rollup is set for 90d and 365d.
	Rollup *StatsRollup `json:"rollup,omitempty"`
}

func page[Totals, Row any](r *http.Request, rng string, totals Totals, rows []Row, rollup *StatsRollup) StatsTable[Totals, Row] {
	n := intParam(r, "page", 1)
	got, pages := accounts.StatsPage(rows, n)
	return StatsTable[Totals, Row]{Range: rng, Totals: totals, Rows: got, Page: min(max(n, 1), pages), Pages: pages, Total: len(rows), Rollup: rollup}
}

func (s *Server) handleStatsTenants(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "tenants")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	t, rollup, err := s.tenantStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(t.Orgs, tenantSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, page(r, req.Range, t.Totals, t.Orgs, rollup))
}

func (s *Server) handleStatsSessions(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "sessions")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	ss, rollup, err := s.sessionStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(ss.Orgs, sessionSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, page(r, req.Range, ss.Totals, ss.Orgs, rollup))
}

var statsRoles = []accounts.Role{accounts.Owner, accounts.Admin, accounts.Member, accounts.Viewer, accounts.Guest}

// handleStatsExport answers one section as CSV or JSON, every row (at most
// accounts.MaxStatsRows), recorded as admin.stats.export.
func (s *Server) handleStatsExport(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, r.URL.Query().Get("section"))
	req.Format = r.URL.Query().Get("format")
	if !slices.Contains([]string{"summary", "tenants", "sessions", "usage"}, req.Section) {
		s.problem(w, http.StatusBadRequest, errors.New("section must be summary, tenants, sessions or usage"))
		return
	}
	if req.Format != "csv" && req.Format != "json" {
		s.problem(w, http.StatusBadRequest, errors.New("format must be csv or json"))
		return
	}
	if req.Section == "summary" {
		req.OrgID = ""
	}
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	ctx := r.Context()
	var (
		body   any
		header []string
		rows   [][]string
	)
	switch req.Section {
	case "summary":
		sum, err := s.statsSummary(ctx, req.Range, q)
		if err != nil {
			s.statsError(w, err)
			return
		}
		body = sum
		header = []string{"figure", "value"}
		for _, f := range []struct {
			name string
			v    int
		}{{"orgs", sum.Orgs}, {"orgs_created", sum.OrgsCreated}, {"users", sum.Users}, {"users_created", sum.UsersCreated},
			{"active_users", sum.ActiveUsers}, {"memberships", sum.Memberships}, {"seats", sum.Seats}, {"seated_people", sum.SeatedPeople},
			{"execution_active", sum.ExecutionActive}, {"execution_created", sum.ExecutionCreated}, {"keys_active", sum.KeysActive}, {"browser_active", sum.BrowserActive}} {
			rows = append(rows, []string{f.name, strconv.Itoa(f.v)})
		}
		if sum.Rollup != nil {
			rows = append(rows, []string{"days", strconv.Itoa(sum.Rollup.Days)}, []string{"days_written", strconv.Itoa(sum.Rollup.Written)})
		}
	case "tenants":
		t, rollup, err := s.tenantStats(ctx, q)
		if err != nil {
			s.statsError(w, err)
			return
		}
		body = withRollup[accounts.TenantStats]{t, rollup}
		noActive := rollup != nil
		header = []string{"slug", "name", "created_at", "members"}
		for _, role := range statsRoles {
			header = append(header, string(role)+"s")
		}
		header = append(header, "seats", "active_members", "teams", "entries", "personal_entries", "private_entries", "lost_entries", "sso", "watches")
		for _, o := range t.Orgs {
			row := []string{csvSafe(o.Slug), csvSafe(o.Name), o.CreatedAt.UTC().Format(time.RFC3339), strconv.Itoa(o.Members)}
			for _, role := range statsRoles {
				row = append(row, strconv.Itoa(o.Roles[string(role)]))
			}
			row = append(row, strconv.Itoa(o.Seats), countCell(o.ActiveMembers, noActive), strconv.Itoa(o.Teams), strconv.Itoa(o.Entries),
				strconv.Itoa(o.PersonalEntries), strconv.Itoa(o.PrivateEntries), strconv.Itoa(o.LostEntries), strconv.FormatBool(o.SSO), strconv.Itoa(o.Watches))
			rows = append(rows, row)
		}
	case "sessions":
		ss, rollup, err := s.sessionStats(ctx, q)
		if err != nil {
			s.statsError(w, err)
			return
		}
		body = withRollup[accounts.SessionStats]{ss, rollup}
		noUsed := rollup != nil
		header = []string{"slug", "name", "execution_active", "execution_created", "execution_revoked", "dashboard", "gateway", "runtime", "keys_active", "keys_used", "keys_expiring"}
		for _, o := range ss.Orgs {
			e, k := o.Execution, o.Keys
			rows = append(rows, []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.Itoa(e.Active), strconv.Itoa(e.Created), strconv.Itoa(e.Revoked),
				strconv.Itoa(e.Dashboard), strconv.Itoa(e.Gateway), strconv.Itoa(e.Runtime), strconv.Itoa(k.Active), countCell(k.Used, noUsed), strconv.Itoa(k.Expiring)})
		}
	case "usage":
		d, err := s.usageStats(ctx, q)
		if err != nil {
			s.statsError(w, err)
			return
		}
		body, header, rows = usageExport(d)
	}
	name := "codesynapse-" + req.Section + "-" + req.Range + "." + req.Format
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if req.Format == "json" {
		writeJSON(w, http.StatusOK, body)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	cw := csv.NewWriter(w)
	_ = cw.Write(header) //nolint:errcheck // client went away
	for _, row := range rows[:min(len(rows), accounts.MaxStatsRows)] {
		_ = cw.Write(row) //nolint:errcheck // as above
	}
	cw.Flush()
}

// withRollup is a section export: the section, and for 90d and 365d where
// its window figures came from.
type withRollup[T any] struct {
	Section T
	Rollup  *StatsRollup
}

// MarshalJSON keeps the section's own fields at the top level, so a short
// range exports exactly as before.
func (v withRollup[T]) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(v.Section)
	if err != nil || v.Rollup == nil {
		return b, err
	}
	r, err := json.Marshal(v.Rollup)
	if err != nil {
		return nil, err
	}
	return append(append(append(b[:len(b)-1], `,"rollup":`...), r...), '}'), nil
}

// countCell writes a count, or nothing for a figure the range cannot answer.
func countCell(n int, unavailable bool) string {
	if unavailable {
		return ""
	}
	return strconv.Itoa(n)
}

// platformAuditedKey marks a batch whose platform panels were already
// authorised and recorded as one view.
type platformAuditedKey struct{}

// platformPanelView authorises and records a cross-tenant series panel:
// with accounts, a platform administrator's signed-in session through
// PlatformStats; without accounts (single tenant), the old flag check.
func (s *Server) platformPanelView(w http.ResponseWriter, r *http.Request, panels []string, rng string) bool {
	if s.cfg.Accounts == nil {
		caller, _ := principal(r.Context())
		if !scopeFrom(r.Context()).all && !caller.User.PlatformAdmin {
			s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
			return false
		}
		return true
	}
	if done, _ := r.Context().Value(platformAuditedKey{}).(bool); done {
		return true
	}
	_, ok := s.platformView(w, r, accounts.StatsRequest{Section: "series", Panel: strings.Join(panels, ","), Range: rng})
	return ok
}
