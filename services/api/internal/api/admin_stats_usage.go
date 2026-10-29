package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// The usage section of the platform statistics (docs/PLATFORM_STATS_PLAN.md,
// phase 3): MCP sessions, calls, tokens, the token share and the hooks'
// workflow counts, per organisation and for the platform. 1h to 30d come
// from the usage buckets (whole UTC days, so the answer names the days it
// covers); 90d and 365d from the daily rollup, which usageStatSource fills
// from the same buckets. Nothing names a repository, a checkout or a person:
// unindexed checkouts are a count of distinct names per organisation, and
// tools and clients are shown only when at least minGroupOrgs organisations
// use them, unless one organisation is selected.

// maxNamedValues is the top N of a breakdown; the rest is "other".
const maxNamedValues = 10

// UsageFigures are one organisation's usage, or the platform's.
type UsageFigures struct {
	Sessions       int `json:"sessions"`
	Calls          int `json:"calls"`
	Errors         int `json:"errors"`
	ResponseTokens int `json:"response_tokens"`
	AvoidedTokens  int `json:"avoided_tokens"`
	WorkTokens     int `json:"work_tokens"`
	// The share of the agents' work tokens avoided (main figure) and the
	// labelled upper figure, as on the Statistics pages.
	ShareAvoidedPct *float64 `json:"share_avoided_pct"`
	ShareUpperPct   *float64 `json:"share_upper_pct,omitempty"`
	ShareKnown      bool     `json:"share_known"`
	ShareBasis      string   `json:"share_basis"`
	// ShareDays and ShareUnknownDays (90d and 365d) count the
	// organisation-days whose share was known and those whose was not;
	// only the known ones are in the share.
	ShareDays        *int `json:"share_days,omitempty"`
	ShareUnknownDays *int `json:"share_unknown_days,omitempty"`

	FirstCodeSynapse int `json:"first_codesynapse"`
	FirstNative      int `json:"first_native"`
	ReadsTurnedBack  int `json:"reads_turned_back"`
	FilesEdited      int `json:"files_edited"`
	FilesEditedFound int `json:"files_edited_found"`
	Bash             int `json:"bash"`
	// UnindexedCheckouts counts distinct checkout names reported as not
	// indexed (never the names); null for 90d and 365d.
	UnindexedCheckouts *int `json:"unindexed_checkouts"`
}

// OrgUsageRow is one organisation's line. An organisation deleted since has
// no slug and reads "deleted organisation".
type OrgUsageRow struct {
	OrgID   string `json:"org_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	UsageFigures
}

// UsageCount is one tool or client family of a breakdown.
type UsageCount struct {
	Name     string `json:"name"`
	Calls    int    `json:"calls"`
	Errors   int    `json:"errors"`
	Sessions int    `json:"sessions"`
}

// UsageSection is the usage section: totals, a page of organisations, and
// the tool and client breakdowns.
type UsageSection struct {
	StatsTable[UsageFigures, OrgUsageRow]
	// From and To are the UTC days the figures cover.
	From    string       `json:"from"`
	To      string       `json:"to"`
	Tools   []UsageCount `json:"tools"`
	Clients []UsageCount `json:"clients"`
}

// usageData is a section before paging.
type usageData struct {
	totals         UsageFigures
	rows           []OrgUsageRow
	tools, clients []UsageCount
	rollup         *StatsRollup
	from, to       string
}

var usageSorts = map[string]func(OrgUsageRow) int{
	"sessions": func(o OrgUsageRow) int { return o.Sessions },
	"calls":    func(o OrgUsageRow) int { return o.Calls },
	"errors":   func(o OrgUsageRow) int { return o.Errors },
	"tokens":   func(o OrgUsageRow) int { return o.ResponseTokens },
	"avoided":  func(o OrgUsageRow) int { return o.AvoidedTokens },
}

func (s *Server) handleStatsUsage(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "usage")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	d, err := s.usageStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(d.rows, usageSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, UsageSection{StatsTable: page(r, req.Range, d.totals, d.rows, d.rollup), From: d.from, To: d.to, Tools: d.tools, Clients: d.clients})
}

func (s *Server) usageStats(ctx context.Context, q accounts.StatsQuery) (usageData, error) {
	if q.Daily != "" {
		return s.dailyUsage(ctx, q)
	}
	return s.liveUsage(ctx, q)
}

// usageExport is the usage section for export: every organisation, without
// the upper share figure, which is shown only beside the main one on the
// page (docs/PLATFORM_STATS_PLAN.md).
func usageExport(d usageData) (body any, header []string, rows [][]string) {
	d.totals.ShareUpperPct = nil
	for i := range d.rows {
		d.rows[i].ShareUpperPct = nil
	}
	body = struct {
		Totals  UsageFigures  `json:"totals"`
		Rows    []OrgUsageRow `json:"rows"`
		Tools   []UsageCount  `json:"tools"`
		Clients []UsageCount  `json:"clients"`
		From    string        `json:"from"`
		To      string        `json:"to"`
		Rollup  *StatsRollup  `json:"rollup,omitempty"`
	}{d.totals, d.rows, d.tools, d.clients, d.from, d.to, d.rollup}
	header = []string{"slug", "name", "deleted", "sessions", "calls", "errors", "response_tokens", "avoided_tokens", "work_tokens", "share_avoided_pct",
		"first_codesynapse", "first_native", "reads_turned_back", "files_edited", "files_edited_found", "bash", "unindexed_checkouts"}
	for _, o := range d.rows {
		share := ""
		if o.ShareAvoidedPct != nil {
			share = strconv.FormatFloat(*o.ShareAvoidedPct, 'f', 1, 64)
		}
		unindexed := ""
		if o.UnindexedCheckouts != nil {
			unindexed = strconv.Itoa(*o.UnindexedCheckouts)
		}
		rows = append(rows, []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.FormatBool(o.Deleted), strconv.Itoa(o.Sessions), strconv.Itoa(o.Calls),
			strconv.Itoa(o.Errors), strconv.Itoa(o.ResponseTokens), strconv.Itoa(o.AvoidedTokens), strconv.Itoa(o.WorkTokens), share,
			strconv.Itoa(o.FirstCodeSynapse), strconv.Itoa(o.FirstNative), strconv.Itoa(o.ReadsTurnedBack), strconv.Itoa(o.FilesEdited),
			strconv.Itoa(o.FilesEditedFound), strconv.Itoa(o.Bash), unindexed})
	}
	return body, header, rows
}

// usageRatios are the benchmark ratios of every indexed repository. They
// only weigh the avoided tokens and never leave the server.
func (s *Server) usageRatios(ctx context.Context) (map[string]float64, float64, error) {
	repos, err := s.st.Repos(ctx)
	if err != nil {
		return nil, 0, err
	}
	sums := make([]RepoSummary, 0, len(repos))
	for _, rn := range repos {
		var m metrics.Report
		if rn.Metrics != "" && json.Unmarshal([]byte(rn.Metrics), &m) == nil {
			sums = append(sums, RepoSummary{Name: rn.Name, GraphTokens: m.Benchmark.TotalGraph, BaseTokens: m.Benchmark.TotalBaseline})
		}
	}
	ratio, workspace := benchmarkRatios(sums)
	return ratio, workspace, nil
}

// usageByOrg reads the buckets of the days from since to now, per
// organisation ("" for calls without one, which count in the platform total
// only).
func (s *Server) usageByOrg(ctx context.Context, since, now time.Time, days int) (map[string]AgentStats, error) {
	buckets, err := s.st.Usage(ctx, usage.Day(since))
	if err != nil {
		return nil, err
	}
	ratio, workspace, err := s.usageRatios(ctx)
	if err != nil {
		return nil, err
	}
	byOrg := map[string][]usage.Bucket{}
	for _, b := range buckets {
		byOrg[b.Org] = append(byOrg[b.Org], b)
	}
	out := make(map[string]AgentStats, len(byOrg))
	for org, bs := range byOrg {
		out[org] = agentStats(bs, days, now, ratio, workspace)
	}
	return out, nil
}

func (s *Server) liveUsage(ctx context.Context, q accounts.StatsQuery) (usageData, error) {
	from, to := accounts.StatDay(q.Since), accounts.StatDay(q.Now)
	days := int(to.Sub(from)/(24*time.Hour)) + 1
	stats, err := s.usageByOrg(ctx, from, q.Now, days)
	if err != nil {
		return usageData{}, err
	}
	d := usageData{from: usage.Day(from), to: usage.Day(to)}
	var all []AgentStats
	tools, clients := newBreakdown(), newBreakdown()
	unindexed := 0
	for org, a := range stats {
		if q.OrgID != "" && org != q.OrgID {
			continue
		}
		all = append(all, a)
		unindexed += len(a.Unindexed.Repos)
		for _, t := range a.Tools {
			tools.add(org, accounts.StatTool(t.Tool), UsageCount{Calls: t.Calls, Errors: t.Errors})
		}
		for _, c := range a.Clients {
			clients.add(org, usage.ClientFamily(c.Client), UsageCount{Calls: c.Calls, Sessions: c.Sessions})
		}
	}
	d.totals = figuresOf(sumAgentStats(all))
	d.totals.UnindexedCheckouts = &unindexed
	d.tools, d.clients = tools.fold(q.OrgID == ""), clients.fold(q.OrgID == "")
	d.rows, err = s.usageRows(ctx, q.OrgID, func(org string) (UsageFigures, bool) {
		a, ok := stats[org]
		if !ok {
			a = agentStats(nil, 1, q.Now, nil, 1)
		}
		f := figuresOf(a)
		n := len(a.Unindexed.Repos)
		f.UnindexedCheckouts = &n
		return f, ok
	}, keysOf(stats))
	return d, err
}

// sumAgentStats adds organisations' figures up. The share is recomputed
// from the summed numerators of the organisations whose share is known, as
// the rollup does for long ranges; an organisation without known shares
// leaves it unknown only when no organisation has one.
func sumAgentStats(all []AgentStats) shareSums {
	var t shareSums
	for _, a := range all {
		t.Sessions += a.Sessions
		t.Calls += a.Calls
		t.Errors += a.Errors
		t.ResponseTokens += a.ResponseTokens
		t.AvoidedTokens += a.AvoidedTokens
		t.WorkTokens += a.WorkTokens
		t.FirstCodeSynapse += a.Workflow.FirstCodeSynapse
		t.FirstNative += a.Workflow.FirstNative
		t.ReadsTurnedBack += a.Workflow.ReadsTurnedBack
		t.FilesEdited += a.Workflow.FilesEdited
		t.FilesEditedFound += a.Workflow.FilesEditedFound
		t.Bash += a.Workflow.BashTotal
		if a.ShareAvoidedKnown {
			t.known++
			t.a += int64(a.ShareAvoidedTokens)
			t.w += int64(a.WorkTokens)
			t.c += int64(a.Tokens.CachedInput)
			t.r += int64(a.AvoidedResendTokens)
		} else if active(a) {
			t.unknown++
		}
	}
	return t
}

// active reports whether an organisation used anything on a day: only then
// does a share it could not compute count as unknown.
func active(a AgentStats) bool {
	return a.Sessions > 0 || a.Calls > 0 || a.Tokens.Total > 0 || a.AvoidedTokens > 0
}

// shareSums are figures added up over organisations or days, with the
// share numerators of the known ones.
type shareSums struct {
	UsageFigures
	a, w, c, r     int64
	known, unknown int
}

// shareBasisKnownOnly: some organisations (1h to 30d) or organisation-days
// (90d, 365d) had no share; only the ones that had one are in it.
const shareBasisKnownOnly = "known_only"

// figuresOf turns sums into figures with their share.
func figuresOf[T AgentStats | shareSums](v T) UsageFigures {
	var sums shareSums
	switch x := any(v).(type) {
	case AgentStats:
		sums = sumAgentStats([]AgentStats{x})
		// One organisation's own share is exactly agentStats', basis
		// included.
		f := sums.UsageFigures
		f.ShareAvoidedPct, f.ShareUpperPct, f.ShareKnown, f.ShareBasis = x.ShareAvoidedPct, x.ShareUpperPct, x.ShareAvoidedKnown, x.ShareAvoidedBasis
		return f
	case shareSums:
		sums = x
	}
	f := sums.UsageFigures
	A, W, C, R := float64(sums.a), float64(sums.w), float64(sums.c), float64(sums.r)
	f.ShareBasis = shareBasisNoTokens
	if sums.known == 0 || W+A == 0 {
		return f
	}
	f.ShareKnown, f.ShareBasis = true, shareBasisWork
	if sums.unknown > 0 {
		f.ShareBasis = shareBasisKnownOnly
	}
	main := round1(100 * A / (W + A))
	upper := max(main, round1(100*(A+R)/(W+C+A+R)))
	f.ShareAvoidedPct, f.ShareUpperPct = &main, &upper
	return f
}

// breakdown collects a tool or client breakdown and the organisations
// behind each value.
type breakdown struct {
	counts map[string]*UsageCount
	orgs   map[string]map[string]bool
}

func newBreakdown() *breakdown {
	return &breakdown{counts: map[string]*UsageCount{}, orgs: map[string]map[string]bool{}}
}

func (b *breakdown) add(org, name string, c UsageCount) {
	if c.Calls == 0 && c.Errors == 0 && c.Sessions == 0 {
		return
	}
	got := b.counts[name]
	if got == nil {
		got = &UsageCount{Name: name}
		b.counts[name] = got
	}
	got.Calls += c.Calls
	got.Errors += c.Errors
	got.Sessions += c.Sessions
	if org != "" {
		if b.orgs[name] == nil {
			b.orgs[name] = map[string]bool{}
		}
		b.orgs[name][org] = true
	}
}

// fold applies the limits: with small, a value fewer than minGroupOrgs
// organisations use goes to "other"; then the top maxNamedValues by calls
// and sessions, the rest in "other", which comes last.
func (b *breakdown) fold(small bool) []UsageCount {
	other := UsageCount{Name: "other"}
	addTo := func(c UsageCount) {
		other.Calls += c.Calls
		other.Errors += c.Errors
		other.Sessions += c.Sessions
	}
	keep := []UsageCount{}
	for name, c := range b.counts {
		if name == "other" || (small && len(b.orgs[name]) < minGroupOrgs) {
			addTo(*c)
			continue
		}
		keep = append(keep, *c)
	}
	slices.SortFunc(keep, func(x, y UsageCount) int {
		return cmp.Or(cmp.Compare(y.Calls+y.Sessions, x.Calls+x.Sessions), cmp.Compare(x.Name, y.Name))
	})
	if len(keep) > maxNamedValues {
		for _, c := range keep[maxNamedValues:] {
			addTo(c)
		}
		keep = keep[:maxNamedValues]
	}
	if other.Calls+other.Errors+other.Sessions > 0 {
		keep = append(keep, other)
	}
	return keep
}

// usageRows lists the organisations: every existing one (only orgID when
// set), then deleted ones that have usage, by name.
func (s *Server) usageRows(ctx context.Context, orgID string, figures func(org string) (UsageFigures, bool), withUsage []string) ([]OrgUsageRow, error) {
	orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	rows := []OrgUsageRow{}
	exists := map[string]bool{}
	for _, o := range orgs {
		exists[o.ID] = true
		if orgID != "" && o.ID != orgID {
			continue
		}
		f, _ := figures(o.ID)
		rows = append(rows, OrgUsageRow{OrgID: o.ID, Slug: o.Slug, Name: o.Name, UsageFigures: f})
	}
	slices.SortStableFunc(rows, func(a, b OrgUsageRow) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Slug, b.Slug))
	})
	slices.Sort(withUsage)
	for _, id := range withUsage {
		if id == "" || exists[id] || (orgID != "" && id != orgID) {
			continue
		}
		if f, ok := figures(id); ok {
			rows = append(rows, OrgUsageRow{OrgID: id, Name: deletedOrg, Deleted: true, UsageFigures: f})
		}
	}
	return rows, nil
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// usageMetrics are the rollup metrics of the usage section.
var usageMetrics = []string{accounts.StatMCPUsage, accounts.StatMCPWorkflow, accounts.StatMCPTools, accounts.StatMCPClients}

// dailyUsage answers 90d and 365d from the rollup: the platform rows of
// each day (to count the days written) and the organisations' rows summed
// over the range.
func (s *Server) dailyUsage(ctx context.Context, q accounts.StatsQuery) (usageData, error) {
	p, ok := principal(ctx)
	if !ok {
		return usageData{}, accounts.ErrForbidden
	}
	from, to, _ := accounts.DailyStatRange(q.Daily, q.Now)
	d := usageData{from: usage.Day(from), to: usage.Day(to.AddDate(0, 0, -1)),
		rollup: &StatsRollup{Days: accounts.DailyStatRanges[q.Daily], Unavailable: []string{"unindexed_checkouts"}}}
	platform, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, []string{accounts.StatMCPUsage, accounts.StatMCPWorkflow}, "", false)
	if err != nil {
		return usageData{}, err
	}
	orgRows, err := s.cfg.Accounts.PlatformStatsTotals(ctx, p, q.Daily, usageMetrics, q.OrgID, q.OrgID == "")
	if err != nil {
		return usageData{}, err
	}
	if len(platform) >= accounts.MaxPlatformStatRows || len(orgRows) >= accounts.MaxPlatformStatRows {
		return usageData{}, errDailyTooLarge
	}
	days := map[time.Time]bool{}
	total := map[[2]string]int64{}
	for _, r := range platform {
		days[r.Day] = true
		if d.rollup.Latest == nil || r.Day.After(*d.rollup.Latest) {
			day := r.Day
			d.rollup.Latest = &day
		}
		total[[2]string{r.Metric, r.Dimension}] += r.Value
	}
	d.rollup.Written = len(days)
	perOrg := map[string]map[[2]string]int64{}
	tools, clients := newBreakdown(), newBreakdown()
	for _, r := range orgRows {
		org := cmp.Or(r.OrgID, q.OrgID)
		if perOrg[org] == nil {
			perOrg[org] = map[[2]string]int64{}
		}
		perOrg[org][[2]string{r.Metric, r.Dimension}] += r.Value
		name, field, ok := strings.Cut(r.Dimension, ".")
		if !ok || (r.Metric != accounts.StatMCPTools && r.Metric != accounts.StatMCPClients) {
			continue
		}
		c := UsageCount{}
		switch field {
		case "calls":
			c.Calls = int(r.Value)
		case "errors":
			c.Errors = int(r.Value)
		case "sessions":
			c.Sessions = int(r.Value)
		}
		if r.Metric == accounts.StatMCPTools {
			tools.add(org, name, c)
		} else {
			clients.add(org, name, c)
		}
	}
	if q.OrgID != "" {
		total = perOrg[q.OrgID]
	}
	d.totals = rolledFigures(total)
	d.tools, d.clients = tools.fold(q.OrgID == ""), clients.fold(q.OrgID == "")
	d.rows, err = s.usageRows(ctx, q.OrgID, func(org string) (UsageFigures, bool) {
		m, ok := perOrg[org]
		return rolledFigures(m), ok
	}, keysOf(perOrg))
	return d, err
}

// rolledFigures are the figures of summed rollup rows (mcp_usage and
// mcp_workflow).
func rolledFigures(m map[[2]string]int64) UsageFigures {
	u := func(d string) int { return int(m[[2]string{accounts.StatMCPUsage, d}]) }
	wf := func(d string) int { return int(m[[2]string{accounts.StatMCPWorkflow, d}]) }
	sums := shareSums{UsageFigures: UsageFigures{
		Sessions: u("sessions"), Calls: u("calls"), Errors: u("errors"), ResponseTokens: u("response_tokens"), AvoidedTokens: u("avoided_tokens"), WorkTokens: u("share_work"),
		FirstCodeSynapse: wf("first_codesynapse"), FirstNative: wf("first_native"), ReadsTurnedBack: wf("reads_turned_back"),
		FilesEdited: wf("files_edited"), FilesEditedFound: wf("files_edited_found"), Bash: wf("bash"),
	}, a: int64(u("share_avoided")), w: int64(u("share_work")), c: int64(u("share_cache_read")), r: int64(u("share_resend")), known: u("share_known"), unknown: u("share_unknown")}
	f := figuresOf(sums)
	known, unknown := sums.known, sums.unknown
	f.ShareDays, f.ShareUnknownDays = &known, &unknown
	return f
}

// usageStatSource adds a day's MCP usage to the daily rollup: per
// organisation and for the platform, the figures of agentStats for that
// day, tools by their fixed names and clients by family. Calls without an
// organisation count in the platform total only.
func (s *Server) usageStatSource(ctx context.Context, day time.Time, set *accounts.PlatformStatSet) error {
	stats, err := s.usageByOrg(ctx, day, day, 1)
	if err != nil {
		return err
	}
	for _, org := range slices.Sorted(func(yield func(string) bool) {
		for k := range stats {
			if !yield(k) {
				return
			}
		}
	}) {
		a := stats[org]
		if !accounts.ValidPlatformStat(accounts.StatMCPUsage, "calls", org) {
			org = "" // an organisation ID the rollup cannot hold counts in the total
		}
		add := func(metric, dim string, n int) {
			if org == "" {
				set.Total(metric, dim, int64(n))
				return
			}
			set.Both(org, metric, dim, int64(n))
		}
		sums := sumAgentStats([]AgentStats{a})
		for dim, n := range map[string]int{
			"sessions": a.Sessions, "calls": a.Calls, "errors": a.Errors, "response_tokens": a.ResponseTokens, "avoided_tokens": a.AvoidedTokens,
			"share_avoided": int(sums.a), "share_work": int(sums.w), "share_cache_read": int(sums.c), "share_resend": int(sums.r),
			"share_known": sums.known, "share_unknown": sums.unknown,
		} {
			add(accounts.StatMCPUsage, dim, n)
		}
		w := a.Workflow
		for dim, n := range map[string]int{
			"first_codesynapse": w.FirstCodeSynapse, "first_native": w.FirstNative, "reads_turned_back": w.ReadsTurnedBack,
			"files_edited": w.FilesEdited, "files_edited_found": w.FilesEditedFound, "bash": w.BashTotal,
		} {
			add(accounts.StatMCPWorkflow, dim, n)
		}
		if org == "" {
			continue
		}
		for _, t := range a.Tools {
			name := accounts.StatTool(t.Tool)
			set.Org(org, accounts.StatMCPTools, name+".calls", int64(t.Calls))
			set.Org(org, accounts.StatMCPTools, name+".errors", int64(t.Errors))
		}
		for _, c := range a.Clients {
			f := usage.ClientFamily(c.Client)
			set.Org(org, accounts.StatMCPClients, f+".calls", int64(c.Calls))
			set.Org(org, accounts.StatMCPClients, f+".sessions", int64(c.Sessions))
		}
	}
	return nil
}
