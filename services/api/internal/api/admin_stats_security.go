package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/history"
)

// The security section of the platform statistics (docs/PLATFORM_STATS_PLAN.md,
// phase 7): counts from the latest snapshot of every graph an organisation
// holds, never a finding's text, path, symbol or a repository name. The
// platform counts each graph once and an organisation each of its entries.
// Rule IDs come from the catalogue only, at platform level only, and are
// named only when at least minGroupOrgs organisations have findings of them.
// 90d and 365d add the daily trend from the rollup (securityStatSource).

// SecurityFigures are the security counts of the platform or one
// organisation. Grades uses accounts.SecurityGradeDims; Worse counts graphs
// whose latest snapshot in the range graded worse than the one before it on
// the same branch.
type SecurityFigures struct {
	Graphs      int            `json:"graphs"`
	Grades      map[string]int `json:"grades"`
	Findings    map[string]int `json:"findings"`
	Vulns       map[string]int `json:"vulns"`
	VulnsCalled int            `json:"vulns_called"`
	Worse       int            `json:"worse"`
	// Suppressed counts the active triage decisions by state, and total.
	Suppressed map[string]int `json:"suppressed"`
}

func newSecurityFigures() SecurityFigures {
	f := SecurityFigures{Grades: map[string]int{}, Findings: map[string]int{}, Vulns: map[string]int{}, Suppressed: map[string]int{}}
	for _, g := range accounts.SecurityGradeDims {
		f.Grades[g] = 0
	}
	for _, sv := range accounts.SecuritySeverities {
		f.Findings[sv], f.Vulns[sv] = 0, 0
	}
	for _, st := range append(slices.Clone(accounts.TriageStates), "total") {
		f.Suppressed[st] = 0
	}
	return f
}

// add counts one graph's snapshot (ok false: the graph has none).
func (f *SecurityFigures) add(g graphSecurity) {
	f.Graphs++
	f.Grades[accounts.SecurityGradeDim(g.cur.Grade, g.ok)]++
	if !g.ok {
		return
	}
	for _, sv := range accounts.SecuritySeverities {
		f.Findings[sv] += g.cur.Findings[sv]
		f.Vulns[sv] += g.cur.Vulns[sv]
	}
	f.VulnsCalled += g.cur.VulnsCalled
	if g.worse {
		f.Worse++
	}
}

func (f *SecurityFigures) suppressed(states map[string]int) {
	for _, st := range accounts.TriageStates {
		f.Suppressed[st] += states[st]
		f.Suppressed["total"] += states[st]
	}
}

// OrgSecurityRow is one organisation's line.
type OrgSecurityRow struct {
	OrgID string `json:"org_id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	SecurityFigures
}

// PlatformRuleCount is the number of open findings of one catalogue rule (or
// "other") across the platform's graphs.
type PlatformRuleCount struct {
	Name     string `json:"name"`
	Findings int    `json:"findings"`
}

// SecurityDay is one day of the rollup's security figures (90d and 365d).
type SecurityDay struct {
	Day      string         `json:"day"`
	Grades   map[string]int `json:"grades"`
	Findings map[string]int `json:"findings"`
	Worse    int            `json:"worse"`
}

// SecuritySection is the security section.
type SecuritySection struct {
	StatsTable[SecurityFigures, OrgSecurityRow]
	// Rules is the platform's breakdown by rule; null when one organisation
	// is selected. RulesUnknown counts graphs whose latest snapshot predates
	// rule counts, so their findings are in no rule.
	Rules        []PlatformRuleCount `json:"rules"`
	RulesUnknown int                 `json:"rules_unknown"`
	Days         []SecurityDay       `json:"days,omitempty"`
}

var securitySorts = map[string]func(OrgSecurityRow) int{
	"graphs":   func(o OrgSecurityRow) int { return o.Graphs },
	"critical": func(o OrgSecurityRow) int { return o.Findings["critical"] },
	"high":     func(o OrgSecurityRow) int { return o.Findings["high"] },
	"vulns":    func(o OrgSecurityRow) int { return sumMap(o.Vulns) },
	"worse":    func(o OrgSecurityRow) int { return o.Worse },
}

func sumMap(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// graphSecurity is what the section reads of one graph.
type graphSecurity struct {
	cur   history.Snapshot
	ok    bool // the graph has a snapshot
	worse bool // its latest snapshot is in the window and graded worse
}

// securityData is a section before paging.
type securityData struct {
	totals       SecurityFigures
	rows         []OrgSecurityRow
	rules        []PlatformRuleCount
	rulesUnknown int
	days         []SecurityDay
	rollup       *StatsRollup
}

func (s *Server) handleStatsSecurity(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "security")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	d, err := s.securityStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(d.rows, securitySorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, SecuritySection{StatsTable: page(r, req.Range, d.totals, d.rows, d.rollup), Rules: d.rules, RulesUnknown: d.rulesUnknown, Days: d.days})
}

// securitySnaps is how many snapshots of a graph are read to find the one
// before the latest on the same branch.
const securitySnaps = 20

// readGraphSecurity reads the latest snapshot of every graph, at most
// statsParallel at a time. worse counts only a latest snapshot taken in
// [since, until).
func (s *Server) readGraphSecurity(ctx context.Context, graphs []string, since, until time.Time) (map[string]graphSecurity, error) {
	out := make(map[string]graphSecurity, len(graphs))
	var mu sync.Mutex
	err := forEach(ctx, len(graphs), func(ctx context.Context, i int) error {
		snaps, err := s.st.Snapshots(ctx, graphs[i], securitySnaps)
		if err != nil {
			return err
		}
		prev, cur, hasPrev := history.Latest(snaps)
		g := graphSecurity{cur: cur, ok: len(snaps) > 0}
		if at, err := time.Parse(time.RFC3339, cur.At); err == nil && hasPrev && !at.Before(since) && at.Before(until) {
			g.worse = history.GradeWorse(prev, cur)
		}
		mu.Lock()
		out[graphs[i]] = g
		mu.Unlock()
		return nil
	})
	return out, err
}

// securityState is the live security state: the organisations, their usable
// entries and the snapshot of every graph behind them.
type securityState struct {
	orgs    []accounts.Org
	entries map[string][]string // organisation ID: graphs
	graphs  map[string]graphSecurity
	triage  map[string]map[string]int
}

func (s *Server) readSecurityState(ctx context.Context, since, until time.Time) (securityState, error) {
	st := securityState{entries: map[string][]string{}}
	var err error
	if st.orgs, err = s.cfg.Accounts.St.ListOrgs(ctx, 100000); err != nil {
		return st, err
	}
	ids := make([]string, len(st.orgs))
	for i, o := range st.orgs {
		ids[i] = o.ID
	}
	repos, err := s.cfg.Accounts.St.RepositoriesForOrgs(ctx, ids)
	if err != nil {
		return st, err
	}
	seen := map[string]bool{}
	var graphs []string
	for _, e := range repos {
		if !e.Usable() {
			continue // hidden from the organisation, so not its posture
		}
		st.entries[e.OrgID] = append(st.entries[e.OrgID], e.Graph)
		if !seen[e.Graph] {
			seen[e.Graph] = true
			graphs = append(graphs, e.Graph)
		}
	}
	slices.Sort(graphs)
	if st.graphs, err = s.readGraphSecurity(ctx, graphs, since, until); err != nil {
		return st, err
	}
	st.triage, err = s.cfg.Accounts.St.TriageCounts(ctx, until)
	return st, err
}

func (s *Server) securityStats(ctx context.Context, q accounts.StatsQuery) (securityData, error) {
	st, err := s.readSecurityState(ctx, q.Since, q.Now)
	if err != nil {
		return securityData{}, err
	}
	d := securityData{totals: newSecurityFigures(), rows: []OrgSecurityRow{}}
	rules := newBreakdown()
	counted := map[string]bool{}
	for _, o := range st.orgs {
		if q.OrgID != "" && o.ID != q.OrgID {
			continue
		}
		row := OrgSecurityRow{OrgID: o.ID, Slug: o.Slug, Name: o.Name, SecurityFigures: newSecurityFigures()}
		for _, g := range st.entries[o.ID] {
			gs := st.graphs[g]
			row.add(gs)
			for rule, n := range gs.cur.Rules {
				markRule(rules, o.ID, rule, n, !counted[g])
			}
			if q.OrgID != "" {
				d.totals.add(gs) // one organisation: its entries
			} else if !counted[g] {
				d.totals.add(gs) // the platform: each graph once
			}
			if !counted[g] && gs.ok && gs.cur.Rules == nil && sumMap(gs.cur.Findings) > 0 {
				d.rulesUnknown++
			}
			counted[g] = true
		}
		row.suppressed(st.triage[o.ID])
		d.totals.suppressed(st.triage[o.ID])
		d.rows = append(d.rows, row)
	}
	slices.SortStableFunc(d.rows, func(a, b OrgSecurityRow) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Slug, b.Slug))
	})
	if q.OrgID == "" {
		d.rules = foldRules(rules)
	} else {
		d.rulesUnknown = 0
	}
	if q.Daily != "" {
		if d.days, d.rollup, err = s.securityDays(ctx, q); err != nil {
			return securityData{}, err
		}
	}
	return d, nil
}

// markRule records that org has findings of rule, and adds the count once
// per graph (first false for a graph already counted for another
// organisation).
func markRule(b *breakdown, org, rule string, n int, first bool) {
	if n <= 0 {
		return
	}
	got := b.counts[rule]
	if got == nil {
		got = &UsageCount{Name: rule}
		b.counts[rule] = got
	}
	if first {
		got.Calls += n
	}
	if b.orgs[rule] == nil {
		b.orgs[rule] = map[string]bool{}
	}
	b.orgs[rule][org] = true
}

// foldRules applies the three-organisation rule and the top ten.
func foldRules(b *breakdown) []PlatformRuleCount {
	out := []PlatformRuleCount{}
	for _, c := range b.fold(true) {
		out = append(out, PlatformRuleCount{Name: c.Name, Findings: c.Calls})
	}
	return out
}

// securityMetrics are the platform rollup metrics of the trend.
var securityMetrics = []string{accounts.StatSecurityGrades, accounts.StatSecurityFindings, accounts.StatSecurityWorse}

// securityDays is the daily trend of a 90d or 365d range: the platform's
// rows, or the selected organisation's.
func (s *Server) securityDays(ctx context.Context, q accounts.StatsQuery) ([]SecurityDay, *StatsRollup, error) {
	p, ok := principal(ctx)
	if !ok {
		return nil, nil, accounts.ErrForbidden
	}
	rows, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, securityMetrics, q.OrgID, false)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) >= accounts.MaxPlatformStatRows {
		return nil, nil, errDailyTooLarge
	}
	rollup := &StatsRollup{Days: accounts.DailyStatRanges[q.Daily], Unavailable: []string{}}
	byDay := map[time.Time]*SecurityDay{}
	for _, r := range rows {
		d := byDay[r.Day]
		if d == nil {
			d = &SecurityDay{Day: r.Day.Format(time.DateOnly), Grades: map[string]int{}, Findings: map[string]int{}}
			byDay[r.Day] = d
		}
		switch r.Metric {
		case accounts.StatSecurityGrades:
			d.Grades[r.Dimension] = int(r.Value)
		case accounts.StatSecurityFindings:
			d.Findings[r.Dimension] = int(r.Value)
		case accounts.StatSecurityWorse:
			d.Worse = int(r.Value)
		}
	}
	days := make([]SecurityDay, 0, len(byDay))
	for day, d := range byDay {
		days = append(days, *d)
		if rollup.Latest == nil || day.After(*rollup.Latest) {
			l := day
			rollup.Latest = &l
		}
	}
	slices.SortFunc(days, func(a, b SecurityDay) int { return strings.Compare(a.Day, b.Day) })
	if q.OrgID == "" {
		rollup.Written = len(days)
	} else {
		// An organisation's rows are written only for days it had any, so
		// the written days are the platform's.
		plat, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, []string{accounts.StatSecurityWorse}, "", false)
		if err != nil {
			return nil, nil, err
		}
		rollup.Written = len(plat)
	}
	return days, rollup, nil
}

// securityExport is the security section for export: every organisation's
// counts, and the platform's rule breakdown in the JSON body.
func securityExport(d securityData) (body any, header []string, rows [][]string) {
	body = struct {
		Totals       SecurityFigures     `json:"totals"`
		Rows         []OrgSecurityRow    `json:"rows"`
		Rules        []PlatformRuleCount `json:"rules"`
		RulesUnknown int                 `json:"rules_unknown"`
		Days         []SecurityDay       `json:"days,omitempty"`
		Rollup       *StatsRollup        `json:"rollup,omitempty"`
	}{d.totals, d.rows, d.rules, d.rulesUnknown, d.days, d.rollup}
	header = []string{"slug", "name", "graphs"}
	for _, g := range accounts.SecurityGradeDims {
		header = append(header, "grade_"+g)
	}
	for _, sv := range accounts.SecuritySeverities {
		header = append(header, "findings_"+sv)
	}
	for _, sv := range accounts.SecuritySeverities {
		header = append(header, "vulns_"+sv)
	}
	header = append(header, "vulns_called", "worse", "suppressed")
	for _, o := range d.rows {
		row := []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.Itoa(o.Graphs)}
		for _, g := range accounts.SecurityGradeDims {
			row = append(row, strconv.Itoa(o.Grades[g]))
		}
		for _, sv := range accounts.SecuritySeverities {
			row = append(row, strconv.Itoa(o.Findings[sv]))
		}
		for _, sv := range accounts.SecuritySeverities {
			row = append(row, strconv.Itoa(o.Vulns[sv]))
		}
		rows = append(rows, append(row, strconv.Itoa(o.VulnsCalled), strconv.Itoa(o.Worse), strconv.Itoa(o.Suppressed["total"])))
	}
	return body, header, rows
}

// securityStatSource adds the security posture at the end of a day to the
// daily rollup: per organisation (each entry) and for the platform (each
// graph once). It runs a few minutes after midnight, so the state read is
// the state the day ended with; worse counts the graphs whose snapshot of
// that day graded worse.
func (s *Server) securityStatSource(ctx context.Context, day time.Time, set *accounts.PlatformStatSet) error {
	st, err := s.readSecurityState(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		return err
	}
	counted := map[string]bool{}
	for _, o := range st.orgs {
		org := o.ID
		if !accounts.ValidPlatformStat(accounts.StatSecurityGrades, "a", org) {
			org = ""
		}
		for _, g := range st.entries[o.ID] {
			gs := st.graphs[g]
			first := !counted[g]
			counted[g] = true
			add := func(metric, dim string, n int) {
				if n <= 0 || !accounts.ValidPlatformStat(metric, dim, "") {
					return
				}
				if org != "" {
					set.Org(org, metric, dim, int64(n))
				}
				if first {
					set.Total(metric, dim, int64(n))
				}
			}
			add(accounts.StatSecurityGrades, accounts.SecurityGradeDim(gs.cur.Grade, gs.ok), 1)
			for _, sv := range accounts.SecuritySeverities {
				add(accounts.StatSecurityFindings, sv, gs.cur.Findings[sv])
				add(accounts.StatSecurityVulns, sv, gs.cur.Vulns[sv])
			}
			add(accounts.StatSecurityVulns, "called", gs.cur.VulnsCalled)
			if gs.worse {
				add(accounts.StatSecurityWorse, "graphs", 1)
			}
			for rule, n := range gs.cur.Rules {
				add(accounts.StatSecurityRules, rule, n)
			}
		}
		total := 0
		for _, state := range accounts.TriageStates {
			n := st.triage[o.ID][state]
			total += n
			if n > 0 {
				if org != "" {
					set.Both(org, accounts.StatSecuritySuppressed, state, int64(n))
				} else {
					set.Total(accounts.StatSecuritySuppressed, state, int64(n))
				}
			}
		}
		if total > 0 {
			if org != "" {
				set.Both(org, accounts.StatSecuritySuppressed, "total", int64(total))
			} else {
				set.Total(accounts.StatSecuritySuppressed, "total", int64(total))
			}
		}
	}
	return nil
}
