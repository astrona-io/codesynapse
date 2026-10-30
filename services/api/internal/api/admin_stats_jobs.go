package api

import (
	"cmp"
	"context"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// The jobs and ingest sections of the platform statistics
// (docs/plans/PLATFORM_STATS_PLAN.md, phase 4). Both are counts per organisation
// and job kind: they never read a job's repository, parameters or error
// text, nor an ingest run's graph, run ID or files, so nothing names what a
// tenant works on. The queue is the platform's (JobQueue); finished jobs
// come from VictoriaMetrics for 1h to 30d and from the daily rollup for 90d
// and 365d; ingest runs from the accounts tables, or the rollup for the
// long ranges.

// JobQueueRow is one job kind of the queue right now.
type JobQueueRow struct {
	Kind    string `json:"kind"`
	Waiting int    `json:"waiting"`
	Active  int    `json:"active"`
	// OldestWaitingSeconds is the age of the oldest job a worker could
	// claim now; null when none waits.
	OldestWaitingSeconds *int `json:"oldest_waiting_seconds"`
}

// JobFigures are finished jobs by outcome.
type JobFigures struct {
	Finished   int `json:"finished"`
	OK         int `json:"ok"`
	Error      int `json:"error"`
	LeftBehind int `json:"left_behind"`
	Cancelled  int `json:"cancelled"`
}

func (f *JobFigures) add(outcome string, n int) {
	switch outcome {
	case telemetry.JobOK:
		f.OK += n
	case telemetry.JobError:
		f.Error += n
	case telemetry.JobLeftBehind:
		f.LeftBehind += n
	case telemetry.JobCancelled:
		f.Cancelled += n
	default:
		return
	}
	f.Finished += n
}

// OrgJobRow is one organisation's finished jobs.
type OrgJobRow struct {
	OrgID   string `json:"org_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	JobFigures
}

// JobKindRow is one job kind's finished jobs.
type JobKindRow struct {
	Kind string `json:"kind"`
	JobFigures
}

// JobsSection is the jobs section. Measured is false when finished jobs
// cannot be read (no metrics store, or it failed); the queue is live.
type JobsSection struct {
	StatsTable[JobFigures, OrgJobRow]
	Queue    []JobQueueRow `json:"queue"`
	Kinds    []JobKindRow  `json:"kinds"`
	Measured bool          `json:"measured"`
}

type jobsData struct {
	totals   JobFigures
	rows     []OrgJobRow
	queue    []JobQueueRow
	kinds    []JobKindRow
	rollup   *StatsRollup
	measured bool
}

var jobSorts = map[string]func(OrgJobRow) int{
	"finished": func(o OrgJobRow) int { return o.Finished },
	"ok":       func(o OrgJobRow) int { return o.OK },
	"error":    func(o OrgJobRow) int { return o.Error },
}

func (s *Server) handleStatsJobs(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "jobs")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	d, err := s.jobsStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(d.rows, jobSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, JobsSection{StatsTable: page(r, req.Range, d.totals, d.rows, d.rollup), Queue: d.queue, Kinds: d.kinds, Measured: d.measured})
}

// jobQueue is the queue per kind, the platform's.
func (s *Server) jobQueue(ctx context.Context, now time.Time) ([]JobQueueRow, error) {
	jq, err := s.cfg.Accounts.St.JobQueue(ctx, now)
	if err != nil {
		return nil, err
	}
	kinds := map[string]bool{}
	for k := range jq.Active {
		kinds[k] = true
	}
	for k := range jq.Waiting {
		kinds[k] = true
	}
	out := []JobQueueRow{}
	for k := range kinds {
		row := JobQueueRow{Kind: telemetry.JobKind(k), Waiting: jq.Waiting[k], Active: jq.Active[k]}
		if t, ok := jq.Oldest[k]; ok && row.Waiting > 0 {
			age := max(int(now.Sub(t).Seconds()), 0)
			row.OldestWaitingSeconds = &age
		}
		out = append(out, row)
	}
	// Unknown kinds share "other"; merge them.
	slices.SortFunc(out, func(a, b JobQueueRow) int { return strings.Compare(a.Kind, b.Kind) })
	merged := out[:0]
	for _, row := range out {
		if n := len(merged); n > 0 && merged[n-1].Kind == row.Kind {
			m := &merged[n-1]
			m.Waiting += row.Waiting
			m.Active += row.Active
			if row.OldestWaitingSeconds != nil && (m.OldestWaitingSeconds == nil || *row.OldestWaitingSeconds > *m.OldestWaitingSeconds) {
				m.OldestWaitingSeconds = row.OldestWaitingSeconds
			}
			continue
		}
		merged = append(merged, row)
	}
	return merged, nil
}

// finishedJobsExpr counts finished jobs per organisation, kind and outcome
// over a window. Jobs without a tenant export as platform_ and count in
// the platform total only.
const finishedJobsExpr = `sum by (tenant_id, kind, outcome) (increase({__name__=~"(platform_)?codesynapse_jobs_finished_total"}[$w]))`

func (s *Server) jobsStats(ctx context.Context, q accounts.StatsQuery) (jobsData, error) {
	queue, err := s.jobQueue(ctx, q.Now)
	if err != nil {
		return jobsData{}, err
	}
	d := jobsData{queue: queue}
	perOrg := map[string]*JobFigures{}
	perKind := map[string]*JobFigures{}
	count := func(org, kind, outcome string, n int) {
		if n <= 0 || (q.OrgID != "" && org != q.OrgID) {
			return
		}
		d.totals.add(outcome, n)
		if perKind[kind] == nil {
			perKind[kind] = &JobFigures{}
		}
		perKind[kind].add(outcome, n)
		if org == "" {
			return
		}
		if perOrg[org] == nil {
			perOrg[org] = &JobFigures{}
		}
		perOrg[org].add(outcome, n)
	}
	if q.Daily != "" {
		roll, _, orgs, err := s.readRollup(ctx, q, []string{accounts.StatJobsFinished})
		if err != nil {
			return jobsData{}, err
		}
		roll.Unavailable = []string{"left_behind", "cancelled"}
		d.rollup, d.measured = &roll, true
		for org, m := range orgs {
			for k, v := range m {
				kind, state, _ := strings.Cut(k[1], ".")
				outcome := map[string]string{"succeeded": telemetry.JobOK, "failed": telemetry.JobError}[state]
				count(org, kind, outcome, int(v))
			}
		}
	} else if s.cfg.Metrics != nil {
		window := strconv.Itoa(int(q.Now.Sub(q.Since).Seconds())) + "s"
		res, err := s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(finishedJobsExpr, "$w", window), q.Now, q.Now, q.Now.Sub(q.Since), nil)
		if err != nil {
			if ctx.Err() != nil {
				return jobsData{}, ctx.Err()
			}
			s.logger.Warn("finished jobs are not available", "error", err)
		} else {
			d.measured = true
			for _, ts := range res {
				if len(ts.Points) == 0 {
					continue
				}
				v := ts.Points[len(ts.Points)-1].V
				if math.IsNaN(v) || math.IsInf(v, 0) {
					continue
				}
				count(ts.Labels["tenant_id"], telemetry.JobKind(ts.Labels["kind"]), ts.Labels["outcome"], int(math.Round(v)))
			}
		}
	}
	d.kinds = []JobKindRow{}
	for k, f := range perKind {
		d.kinds = append(d.kinds, JobKindRow{Kind: k, JobFigures: *f})
	}
	slices.SortFunc(d.kinds, func(a, b JobKindRow) int {
		return cmp.Or(cmp.Compare(b.Finished, a.Finished), strings.Compare(a.Kind, b.Kind))
	})
	orgs, err := s.statsOrgs(ctx, q.OrgID, keysOf(perOrg))
	if err != nil {
		return jobsData{}, err
	}
	d.rows = make([]OrgJobRow, 0, len(orgs))
	for _, o := range orgs {
		row := OrgJobRow{OrgID: o.id, Slug: o.slug, Name: o.name, Deleted: o.deleted}
		if f := perOrg[o.id]; f != nil {
			row.JobFigures = *f
		}
		d.rows = append(d.rows, row)
	}
	return d, nil
}

func jobsExport(d jobsData) (body any, header []string, rows [][]string) {
	body = struct {
		Totals   JobFigures    `json:"totals"`
		Rows     []OrgJobRow   `json:"rows"`
		Queue    []JobQueueRow `json:"queue"`
		Kinds    []JobKindRow  `json:"kinds"`
		Measured bool          `json:"measured"`
		Rollup   *StatsRollup  `json:"rollup,omitempty"`
	}{d.totals, d.rows, d.queue, d.kinds, d.measured, d.rollup}
	header = []string{"slug", "name", "deleted", "finished", "ok", "error", "left_behind", "cancelled"}
	partial := d.rollup != nil
	for _, o := range d.rows {
		rows = append(rows, []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.FormatBool(o.Deleted), countCell(o.Finished, !d.measured),
			countCell(o.OK, !d.measured), countCell(o.Error, !d.measured), countCell(o.LeftBehind, partial || !d.measured), countCell(o.Cancelled, partial || !d.measured)})
	}
	return body, header, rows
}

// OrgIngestRow is one organisation's ingest runs and files.
type OrgIngestRow struct {
	OrgID   string `json:"org_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	accounts.IngestFigures
}

// IngestSection is the ingest section. Running is the runs not finished
// now, whatever the range.
type IngestSection struct {
	StatsTable[accounts.IngestFigures, OrgIngestRow]
	// Formats is the platform's indexed files by language or format as of
	// now, whatever the range; null when one organisation is selected.
	// FormatsUnknown counts graphs whose latest snapshot has no format
	// counts, so their files are in no format.
	Formats        []PlatformFormatCount `json:"formats"`
	FormatsUnknown int                   `json:"formats_unknown"`
}

type ingestData struct {
	totals         accounts.IngestFigures
	rows           []OrgIngestRow
	rollup         *StatsRollup
	formats        []PlatformFormatCount
	formatsUnknown int
}

var ingestSorts = map[string]func(OrgIngestRow) int{
	"runs":    func(o OrgIngestRow) int { return o.Succeeded + o.Failed + o.Cancelled },
	"failed":  func(o OrgIngestRow) int { return o.Failed },
	"scanned": func(o OrgIngestRow) int { return o.Scanned },
	"indexed": func(o OrgIngestRow) int { return o.Indexed },
}

func (s *Server) handleStatsIngest(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "ingest")
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	d, err := s.ingestStats(r.Context(), q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	sortRows(d.rows, ingestSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, IngestSection{StatsTable: page(r, req.Range, d.totals, d.rows, d.rollup), Formats: d.formats, FormatsUnknown: d.formatsUnknown})
}

func (s *Server) ingestStats(ctx context.Context, q accounts.StatsQuery) (ingestData, error) {
	var d ingestData
	perOrg := map[string]accounts.IngestFigures{}
	add := func(org string, f accounts.IngestFigures) {
		if q.OrgID != "" && org != q.OrgID {
			return
		}
		d.totals.Add(f)
		g := perOrg[org]
		g.Add(f)
		perOrg[org] = g
	}
	if q.Daily != "" {
		roll, platform, orgs, err := s.readRollup(ctx, q, []string{accounts.StatIngestRuns, accounts.StatIngestFiles, accounts.StatIngestSkips})
		if err != nil {
			return ingestData{}, err
		}
		d.rollup = &roll
		if q.OrgID == "" {
			// Deleted organisations' days stay in the platform rows only.
			d.totals = accounts.IngestFiguresOf(func(m, dim string) int { return int(platform[[2]string{m, dim}]) })
		}
		for org, m := range orgs {
			f := accounts.IngestFiguresOf(func(metric, dim string) int { return int(m[[2]string{metric, dim}]) })
			if q.OrgID != "" {
				add(org, f)
			} else {
				perOrg[org] = f
			}
		}
		// What runs now is live, as on the short ranges.
		running, err := s.cfg.Accounts.St.IngestStats(ctx, q.Now, q.Now)
		if err != nil {
			return ingestData{}, err
		}
		for _, r := range running {
			if q.OrgID != "" && r.OrgID != q.OrgID {
				continue
			}
			d.totals.Running += r.Running
			if r.OrgExists {
				f := perOrg[r.OrgID]
				f.Running += r.Running
				perOrg[r.OrgID] = f
			}
		}
	} else {
		rows, err := s.cfg.Accounts.St.IngestStats(ctx, q.Since, q.Now)
		if err != nil {
			return ingestData{}, err
		}
		for _, r := range rows {
			add(r.OrgID, r.IngestFigures)
		}
	}
	orgs, err := s.statsOrgs(ctx, q.OrgID, keysOf(perOrg))
	if err != nil {
		return ingestData{}, err
	}
	d.rows = make([]OrgIngestRow, 0, len(orgs))
	for _, o := range orgs {
		d.rows = append(d.rows, OrgIngestRow{OrgID: o.id, Slug: o.slug, Name: o.name, Deleted: o.deleted, IngestFigures: perOrg[o.id]})
	}
	if err := s.ingestFormats(ctx, q, &d); err != nil {
		return ingestData{}, err
	}
	return d, nil
}

type ingestBody struct {
	Totals         accounts.IngestFigures `json:"totals"`
	Rows           []OrgIngestRow         `json:"rows"`
	Formats        []PlatformFormatCount  `json:"formats"`
	FormatsUnknown int                    `json:"formats_unknown"`
}

func ingestExport(d ingestData) (body any, header []string, rows [][]string) {
	body = withRollup[ingestBody]{ingestBody{d.totals, d.rows, d.formats, d.formatsUnknown}, d.rollup}
	header = []string{"slug", "name", "deleted", "succeeded", "failed", "cancelled", "running", "without_report", "incomplete_discovery",
		"scanned", "indexed", "skipped", "files_failed", "unprocessed", "skip_ignored_path", "skip_generated_code", "skip_binary", "skip_non_k8s_yaml"}
	for _, o := range d.rows {
		row := []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.FormatBool(o.Deleted)}
		for _, n := range []int{o.Succeeded, o.Failed, o.Cancelled, o.Running, o.WithoutReport, o.IncompleteDiscovery, o.Scanned, o.Indexed, o.Skipped,
			o.FilesFailed, o.Unprocessed, o.Skips.IgnoredPath, o.Skips.GeneratedCode, o.Skips.Binary, o.Skips.NonK8sYAML} {
			row = append(row, strconv.Itoa(n))
		}
		rows = append(rows, row)
	}
	return body, header, rows
}

// statsOrg is an organisation row's name: an existing organisation, or one
// deleted since that still has figures.
type statsOrg struct {
	id, slug, name string
	deleted        bool
}

// statsOrgs lists every existing organisation (only orgID's when set) by
// name, then the deleted ones among withData.
func (s *Server) statsOrgs(ctx context.Context, orgID string, withData []string) ([]statsOrg, error) {
	orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	out := []statsOrg{}
	exists := map[string]bool{}
	for _, o := range orgs {
		exists[o.ID] = true
		if orgID == "" || o.ID == orgID {
			out = append(out, statsOrg{id: o.ID, slug: o.Slug, name: o.Name})
		}
	}
	slices.SortStableFunc(out, func(a, b statsOrg) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.slug, b.slug))
	})
	slices.Sort(withData)
	for _, id := range withData {
		if id != "" && !exists[id] && (orgID == "" || id == orgID) {
			out = append(out, statsOrg{id: id, name: deletedOrg, deleted: true})
		}
	}
	return out, nil
}

// readRollup reads metrics of a 90d or 365d range: which days are written,
// the platform totals, and the organisations' sums (only q.OrgID's when
// set).
func (s *Server) readRollup(ctx context.Context, q accounts.StatsQuery, metrics []string) (StatsRollup, map[[2]string]int64, map[string]map[[2]string]int64, error) {
	p, ok := principal(ctx)
	if !ok {
		return StatsRollup{}, nil, nil, accounts.ErrForbidden
	}
	roll := StatsRollup{Days: accounts.DailyStatRanges[q.Daily], Unavailable: []string{}}
	platform, err := s.cfg.Accounts.PlatformStatsHistory(ctx, p, q.Daily, metrics, "", false)
	if err != nil {
		return roll, nil, nil, err
	}
	orgRows, err := s.cfg.Accounts.PlatformStatsTotals(ctx, p, q.Daily, metrics, q.OrgID, q.OrgID == "")
	if err != nil {
		return roll, nil, nil, err
	}
	if len(platform) >= accounts.MaxPlatformStatRows || len(orgRows) >= accounts.MaxPlatformStatRows {
		return roll, nil, nil, errDailyTooLarge
	}
	days := map[time.Time]bool{}
	total := map[[2]string]int64{}
	for _, r := range platform {
		days[r.Day] = true
		if roll.Latest == nil || r.Day.After(*roll.Latest) {
			day := r.Day
			roll.Latest = &day
		}
		total[[2]string{r.Metric, r.Dimension}] += r.Value
	}
	roll.Written = len(days)
	perOrg := map[string]map[[2]string]int64{}
	for _, r := range orgRows {
		org := cmp.Or(r.OrgID, q.OrgID)
		if perOrg[org] == nil {
			perOrg[org] = map[[2]string]int64{}
		}
		perOrg[org][[2]string{r.Metric, r.Dimension}] += r.Value
	}
	return roll, total, perOrg, nil
}

// jobTiles fills the summary's job tiles: what waits now, and the jobs that
// failed in the range (null when finished jobs cannot be read).
func (s *Server) jobTiles(ctx context.Context, q accounts.StatsQuery, sum *StatsSummary) {
	d, err := s.jobsStats(ctx, q)
	if err != nil {
		s.logger.Warn("the job tiles are not available", "error", err)
		return
	}
	waiting := 0
	for _, row := range d.queue {
		waiting += row.Waiting
	}
	sum.JobsWaiting = &waiting
	if d.measured {
		failed := d.totals.Error
		sum.JobsFailed = &failed
	}
}
