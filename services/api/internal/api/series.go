package api

import (
	"cmp"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Time-series statistics from VictoriaMetrics. Panels are fixed MetricsQL
// queries; the only value interpolated is the step, from seriesRanges.
// Percentiles use increase(), not rate(): the quantile is the same, and
// VictoriaMetrics' rate() reads 0 for a series born inside the window (a
// tool's first call), which increase() counts from zero.
// Tenant scope is not part of the query text: Querier.QueryRange passes it
// as extra_filters, which VictoriaMetrics applies to every series.

type seriesRange struct {
	span, step time.Duration
	window     string // the step as a MetricsQL duration
}

var seriesRanges = map[string]seriesRange{
	"1h":  {time.Hour, time.Minute, "1m"},
	"6h":  {6 * time.Hour, 5 * time.Minute, "5m"},
	"24h": {24 * time.Hour, 15 * time.Minute, "15m"},
	"7d":  {7 * 24 * time.Hour, time.Hour, "1h"},
	"30d": {30 * 24 * time.Hour, 6 * time.Hour, "6h"},
}

type seriesQuery struct {
	name string // series name, or "" to name series by the by label
	expr string // $w is replaced by the step window
	by   string // label naming each series
}

type seriesPanel struct {
	Title, Unit, Help, Empty string
	// Kind is "count" (per step) or "gauge" (latency, a level per step).
	Kind       string
	queries    []seriesQuery
	platform   bool // cross-tenant: platform admins only
	repository bool
}

// OTLP can split related gauges across HTTP batches. Only observations whose
// count and freshness samples came from the same export are eligible.
const memoryObserved = `agent_memory_facts_observed_at_seconds and (timestamp(agent_memory_facts_count) == timestamp(agent_memory_facts_observed_at_seconds))`

var seriesPanels = map[string]seriesPanel{
	"annotation_jobs": {Title: "Organisation annotation outcomes", Unit: "jobs", Kind: "count",
		Help:    "Local annotation jobs across the selected organisations, separate from ingestion outcomes. This panel is not repository-specific.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(codesynapse_annotation_jobs_total[$w]))`, by: "outcome"}}},
	"ingest_files": {Title: "File processing across runs", Unit: "files", Kind: "count", repository: true,
		Help:    "Persisted file accounting: encountered files and confirmed file writes. Exact counts belong to each ingestion report; these increases span runs.",
		queries: []seriesQuery{{name: "scanned", expr: `sum(increase(codesynapse_files_scanned_total[$w]))`}, {name: "written", expr: `sum(increase(codesynapse_files_indexed_total[$w]))`}}},
	"ingest_skips": {Title: "Whole-file exclusions across runs", Unit: "files", Kind: "count", repository: true,
		Help:    "Whole-file skip decisions from persisted report checkpoints. Kubernetes-only exclusions are stage decisions and do not count as skipped files.",
		queries: []seriesQuery{{expr: `sum by (skip_reason) (increase(codesynapse_files_skipped_total[$w]))`, by: "skip_reason"}}},
	// Background jobs and indexing (docs/TELEMETRY_PLAN.md, phase 3), for
	// Statistics → Sync. Outcomes: ok, error, left_behind, cancelled.
	"jobs_finished": {Title: "Finished jobs", Unit: "jobs", Kind: "count",
		Help:    "Indexing, sync, AI and fix jobs that ended, by outcome. Left behind means another process resumes the job.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(codesynapse_jobs_finished_total[$w]))`, by: "outcome"}}},
	"job_failures": {Title: "Failed jobs", Unit: "jobs", Kind: "count",
		Help:    "Jobs that ended in an error, by kind. Cancelled jobs and jobs left for another process are not failures.",
		Empty:   "No jobs failed in this window.",
		queries: []seriesQuery{{expr: `sum by (kind) (increase(codesynapse_jobs_finished_total{outcome="error"}[$w]))`, by: "kind"}}},
	"job_wait": {Title: "Job queue wait", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from queueing a job to a process starting it, by kind.",
		Empty:   "No jobs started in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase(codesynapse_job_wait_seconds_bucket[$w])))`, by: "kind"}}},
	"job_duration": {Title: "Job run time", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from a process starting a job to its end, by kind.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase(codesynapse_job_duration_seconds_bucket[$w])))`, by: "kind"}}},
	"ingest_runs": {Title: "Indexing runs", Unit: "runs", Kind: "count",
		Help:    "Repository indexing runs by outcome.",
		Empty:   "No repository was indexed in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(ingest_runs_total[$w]))`, by: "outcome"}}},
	"ingest_duration": {Title: "Indexing time", Unit: "seconds", Kind: "gauge",
		Help:  "Median and 95th percentile time to extract and store a repository.",
		Empty: "No repository was indexed in this window.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(ingest_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(ingest_duration_seconds_bucket[$w])))`},
		}},
	"ingest_phases": {Title: "Indexing phase time", Unit: "seconds", Kind: "gauge",
		Help:    "Average time per run of each indexing phase: prepare (clone or fetch), ingest (extraction and analysis) and publish (storing the graph).",
		Empty:   "No indexing phase ran in this window.",
		queries: []seriesQuery{{expr: `sum by (phase) (increase(codesynapse_ingest_phase_seconds_total[$w])) / (sum by (phase) (increase(codesynapse_ingest_phase_total[$w])) > 0)`, by: "phase"}}},
	"ingest_extract": {Title: "Extraction time by language", Unit: "seconds", Kind: "count",
		Help:    "Time spent parsing each language while indexing. JavaScript, TypeScript and Vue share one parser, as do Java and Kotlin.",
		Empty:   "No extraction ran in this window.",
		queries: []seriesQuery{{expr: `sum by (language) (increase(codesynapse_ingest_extract_seconds_total[$w]))`, by: "language"}}},
	"tool_calls": {Title: "Tool calls", Unit: "calls", Kind: "count",
		Help:    "MCP tool and prompt calls per step, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total[$w]))`, by: "mcp_tool"}}},
	"tool_errors": {Title: "Failed tool calls", Unit: "calls", Kind: "count",
		Help:    "Calls that returned an error, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total{outcome="error"}[$w]))`, by: "mcp_tool"}}},
	"sessions": {Title: "Agent sessions", Unit: "sessions", Kind: "count",
		Help:    "MCP sessions started per step.",
		queries: []seriesQuery{{name: "sessions", expr: `sum(increase(mcp_sessions_total[$w]))`}}},
	"agent_hook_events": {Title: "Assistant hook activity", Unit: "events", Kind: "count",
		Help:    "Accepted client-reported lifecycle observations by event; separate from MCP calls.",
		queries: []seriesQuery{{expr: `sum by (event) (increase(agent_hook_events_total[$w]))`, by: "event"}}},
	"agent_hook_bytes": {Title: "Assistant observed bytes", Unit: "bytes", Kind: "count",
		Help:    "Bytes seen in hook fields by kind. Content is never stored; repeated observations may count the same content again.",
		queries: []seriesQuery{{expr: `sum by (kind) (increase(agent_hook_bytes_total[$w]))`, by: "kind"}}},
	"tool_latency": {Title: "Tool latency", Unit: "seconds", Kind: "gauge",
		Help: "Median and 95th percentile time to answer a tool call.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
		}},
	"served_tokens": {Title: "Tokens served to agents", Unit: "tokens", Kind: "count",
		Help:    "Estimated tokens MCP tools returned, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_response_tokens_total[$w]))`, by: "mcp_tool"}}},
	"llm_tokens": {Title: "AI model tokens", Unit: "tokens", Kind: "count",
		Help: "Tokens CodeSynapse's own AI tasks sent and received; cached = input tokens served from the provider's prompt cache; those tokens may still be billed.",
		queries: []seriesQuery{
			{name: "input", expr: `sum(increase(llm_tokens_input_total[$w]))`},
			{name: "output", expr: `sum(increase(llm_tokens_output_total[$w]))`},
			{name: "cached", expr: `sum(increase(llm_tokens_saved_total[$w]))`},
		}},
	// By provider, for the AI page: jobs run in the worker, whose calls the API
	// process's own counters (llm.Registry.Usage) never see.
	"llm_provider_requests": {Title: "AI requests by provider", Unit: "requests", Kind: "count",
		Help:    "Requests CodeSynapse's AI tasks made to each model provider, from every process.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_requests_total[$w]))`, by: "llm_provider"}}},
	"llm_provider_failures": {Title: "Failed AI requests by provider", Unit: "requests", Kind: "count",
		Help:    "Requests that ended in an error, by model provider.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_requests_total{outcome="error"}[$w]))`, by: "llm_provider"}}},
	"llm_provider_input": {Title: "AI input tokens by provider", Unit: "tokens", Kind: "count",
		Help:    "Prompt tokens sent to each model provider.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_tokens_input_total[$w]))`, by: "llm_provider"}}},
	"llm_provider_output": {Title: "AI output tokens by provider", Unit: "tokens", Kind: "count",
		Help:    "Completion tokens each model provider returned.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_tokens_output_total[$w]))`, by: "llm_provider"}}},
	"llm_cost": {Title: "AI model cost (estimate)", Unit: "USD", Kind: "count",
		Help:    "Estimated from configured input, output and cache prices. Calls with missing usage or prices are excluded; see measurement gaps.",
		queries: []seriesQuery{{expr: `sum by (llm_model) (increase(llm_cost_estimated_dollars[$w]))`, by: "llm_model"}}},
	"llm_savings": {Title: "Prompt-cache cost savings (estimate)", Unit: "USD", Kind: "count",
		Help:    "Uncached input cost minus cache-read cost at the prices configured when the call ran. Missing prices or usage are excluded.",
		queries: []seriesQuery{{name: "estimated savings", expr: `sum(increase(llm_cost_saved_estimated_dollars_total[$w]))`}}},
	"llm_coverage": {Title: "Model measurement gaps", Unit: "calls", Kind: "count",
		Help: "Calls without reported token usage or configured prices. Their cost is unknown.",
		queries: []seriesQuery{
			{name: "usage unknown", expr: `sum(increase(llm_usage_unknown_total[$w]))`},
			{name: "prices missing", expr: `sum(increase(llm_pricing_missing_total[$w]))`},
		}},
	"bandwidth": {Title: "MCP payload bandwidth", Unit: "bytes/s", Kind: "gauge",
		Help: "Request bytes read and response bytes written, excluding HTTP headers, TLS and TCP overhead.",
		queries: []seriesQuery{
			{name: "received", expr: `sum(rate(net_bytes_received_total[$w]))`},
			{name: "sent", expr: `sum(rate(net_bytes_sent_total[$w]))`},
		}},
	"cloud_traffic": {Title: "Cloud model request payloads", Unit: "bytes", Kind: "count",
		Help:    "Encoded cloud request bytes consumed by HTTP transports, including retry attempts. Local-only calls do not imply savings.",
		queries: []seriesQuery{{name: "sent", expr: `sum(increase(cloud_bytes_sent_total[$w]))`}}},
	"cloud_savings": {Title: "Cloud payload avoided (estimate)", Unit: "%", Kind: "gauge",
		Help:    "Requires an equivalent configured cloud fallback and measured serialized payload. Provider prompt caching does not imply avoided network bytes.",
		Empty:   "Not measured: no comparable cloud fallback traffic in this window.",
		queries: []seriesQuery{{name: "avoided", expr: `100 * sum(increase(cloud_bytes_saved_total[$w])) / ((sum(increase(cloud_bytes_sent_total[$w])) + sum(increase(cloud_bytes_saved_total[$w]))) > 0)`}}},
	"llm_ttft": {Title: "AI time to first token", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile wait for the first streamed token, by model.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, llm_model) (increase(llm_time_to_first_token_seconds_bucket[$w])))`, by: "llm_model"}}},
	"db_latency": {Title: "Database latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile round trip by database.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, db_system) (increase(db_query_duration_seconds_bucket[$w])))`, by: "db_system"}}},
	"db_latency_tail": {Title: "Database latency distribution", Unit: "seconds", Kind: "gauge",
		Help: "Client-observed latency across database requests. These measurements do not establish an SLA guarantee.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
			{name: "p99", expr: `histogram_quantile(0.99, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
		}},
	"db_requests": {Title: "Database requests", Unit: "requests", Kind: "count",
		Help:    "Measured database operations by backend, including failed requests.",
		queries: []seriesQuery{{expr: `sum by (db_system) (increase(db_query_duration_seconds_count[$w]))`, by: "db_system"}}},
	"db_errors": {Title: "Database errors and timeouts", Unit: "requests", Kind: "count",
		Help: "Failed database requests. Timeouts are a subset of errors, so these series must not be added together.",
		queries: []seriesQuery{
			{name: "errors", expr: `sum(increase(db_query_errors_total[$w]))`},
			{name: "timeouts", expr: `sum(increase(db_query_timeouts_total[$w]))`},
		}},
	"db_rows": {Title: "Graph rows and vector matches", Unit: "rows", Kind: "count",
		Help: "Rows graph reads returned and vectors vector searches matched.",
		queries: []seriesQuery{
			{expr: `sum by (db_system) (increase(db_graph_rows_returned_total[$w]))`, by: "db_system"},
			{name: "vector matches", expr: `sum(increase(db_vector_matches_count[$w]))`},
		}},
	"memory": {Title: "Agent memory", Unit: "events", Kind: "count",
		Help: "Facts saved (by agents or extracted from offloaded turns), turns offloaded and recalls.",
		queries: []seriesQuery{
			{name: "facts saved", expr: `sum(increase(memory_facts_saved_total[$w]))`},
			{name: "turns offloaded", expr: `sum(increase(memory_offload_turns_total[$w]))`},
			{name: "recalls", expr: `sum(increase(agent_memory_recalls_total[$w]))`},
		}},
	"memory_facts": {Title: "Durable memory facts", Unit: "facts", Kind: "gauge",
		Help:    "Current unique stored facts per organisation. Replicas are deduplicated; observations older than 90 seconds are omitted. Deletion and deduplication can lower the count.",
		Empty:   "No fresh durable-memory count is available in this window.",
		queries: []seriesQuery{{name: "stored facts", expr: `sum(max by (tenant_id) (agent_memory_facts_count and ((` + memoryObserved + `) == on (tenant_id) group_left max by (tenant_id) (` + memoryObserved + `)) and ((` + memoryObserved + `) > (time() - 90))))`}}},
	"tenants": {Title: "Tool calls by organisation", Unit: "calls", Kind: "count", platform: true,
		Help:    "Cross-tenant activity (platform admins only).",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(mcp_tool_calls_total[$w]))`, by: "tenant_id"}}},
}

func init() {
	for _, name := range []string{"llm_cost", "tool_latency", "db_latency"} {
		panel := seriesPanels[name]
		panel.platform = true
		seriesPanels["platform_"+name] = panel
	}
}

// maxNamedSeries matches the chart palette (four hues, never cycled): the
// rest folds into one neutral "other".
const maxNamedSeries = 4

// SeriesPanel is one chart's data, timestamps aligned across series.
type SeriesPanel struct {
	Panel  string       `json:"panel"`
	Title  string       `json:"title"`
	Unit   string       `json:"unit"`
	Kind   string       `json:"kind"`
	Help   string       `json:"help"`
	Empty  string       `json:"empty"`
	Range  string       `json:"range"`
	Step   int          `json:"step_seconds"`
	Times  []int64      `json:"times"`
	Series []SeriesLine `json:"series"`
}

// SeriesLine is one named series; nil values are steps without data.
type SeriesLine struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
	Total  float64    `json:"total"`
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)"))
		return
	}
	name, rng := r.URL.Query().Get("panel"), cmp.Or(r.URL.Query().Get("range"), "24h")
	p, ok := seriesPanels[name]
	if !ok {
		s.problem(w, http.StatusBadRequest, errors.New("unknown panel"))
		return
	}
	sr, ok := seriesRanges[rng]
	if !ok {
		s.problem(w, http.StatusBadRequest, errors.New("range must be 1h, 6h, 24h, 7d or 30d"))
		return
	}
	sc := scopeFrom(r.Context())
	var tenants []string // nil: every tenant (platform admins, single-tenant installs)
	caller, _ := principal(r.Context())
	if p.platform && !sc.all && !caller.User.PlatformAdmin {
		s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
		return
	}
	if !p.platform {
		tenants = callerTenants(r)
	}
	repositoryID := ""
	if p.repository {
		if s.cfg.Accounts == nil {
			s.accountError(w, accounts.ErrNotFound)
			return
		}
		contentScope, err := s.scopeOf(r, caller)
		if err != nil {
			s.accountError(w, err)
			return
		}
		repo := r.URL.Query().Get("repo")
		reach, ok := contentScope.reach[repo]
		if !ok || reach.Ambiguous || !reach.Role.AtLeast(accounts.Viewer) {
			s.accountError(w, accounts.ErrNotFound)
			return
		}
		runs, err := s.cfg.Accounts.St.ListIngestRuns(r.Context(), reach.OrgID, repo, 1)
		if err != nil {
			s.accountError(w, err)
			return
		}
		if len(runs) == 0 || !runs[0].MetricCoverage {
			s.problem(w, http.StatusConflict, errors.New("repository ingestion metrics are not available; use the exact ingestion report"))
			return
		}
		repositoryID = runs[0].RepositoryID
		tenants = []string{reach.OrgID}
	}
	// The last step is the current one, still filling: recent activity
	// shows at once instead of when the step completes.
	end := time.Now().Truncate(sr.step).Add(sr.step)
	if p.Kind == "gauge" {
		// Freshness uses evaluation time; a future step would hide current gauges.
		end = time.Now().Truncate(time.Second)
	}
	start := end.Add(-sr.span)
	out := SeriesPanel{Panel: name, Title: p.Title, Unit: p.Unit, Kind: p.Kind, Help: p.Help, Empty: cmp.Or(p.Empty, "No measurements recorded in this window."), Range: rng, Step: int(sr.step / time.Second), Series: []SeriesLine{}}
	for t := start.Add(sr.step); !t.After(end); t = t.Add(sr.step) {
		out.Times = append(out.Times, t.Unix())
	}
	index := make(map[int64]int, len(out.Times))
	for i, t := range out.Times {
		index[t] = i
	}
	for _, q := range p.queries {
		var res []telemetry.Series
		var err error
		if p.repository {
			res, err = s.cfg.Metrics.QueryRangeForRepo(r.Context(), strings.ReplaceAll(q.expr, "$w", sr.window), start.Add(sr.step), end, sr.step, tenants, repositoryID)
		} else {
			res, err = s.cfg.Metrics.QueryRange(r.Context(), strings.ReplaceAll(q.expr, "$w", sr.window), start.Add(sr.step), end, sr.step, tenants)
		}
		if err != nil {
			s.problem(w, http.StatusBadGateway, err)
			return
		}
		for _, ts := range res {
			line := SeriesLine{Name: cmp.Or(q.name, ts.Labels[q.by], "none"), Values: make([]*float64, len(out.Times))}
			for _, pt := range ts.Points {
				if i, ok := index[pt.T]; ok && !math.IsInf(pt.V, 0) && !math.IsNaN(pt.V) {
					v := pt.V
					line.Values[i] = &v
					if p.Kind == "count" {
						line.Total += v
					} else {
						line.Total = math.Max(line.Total, v)
					}
				}
			}
			if p.Kind == "count" {
				// A step without samples had no activity: 0, so lines span
				// the window. Levels (latency) keep their gaps.
				for i, v := range line.Values {
					if v == nil {
						zero := 0.0
						line.Values[i] = &zero
					}
				}
			}
			out.Series = append(out.Series, line)
		}
	}
	out.Series = foldSeries(out.Series, p.Kind, len(out.Times))
	writeJSON(w, http.StatusOK, out)
}

// foldSeries keeps the largest series, ordered by name so a series keeps
// its colour while the same set is shown (colour follows the entity, not
// its rank), and sums the rest into "other" (counts only; levels such as
// latency cannot be summed and are dropped).
func foldSeries(lines []SeriesLine, kind string, n int) []SeriesLine {
	slices.SortStableFunc(lines, func(a, b SeriesLine) int { return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(a.Name, b.Name)) })
	byName := func(ls []SeriesLine) []SeriesLine {
		slices.SortStableFunc(ls, func(a, b SeriesLine) int { return cmp.Compare(a.Name, b.Name) })
		return ls
	}
	if len(lines) <= maxNamedSeries {
		return byName(lines)
	}
	keep := byName(slices.Clone(lines[:maxNamedSeries])) // appending must not overwrite lines[maxNamedSeries]
	if kind != "count" {
		return keep
	}
	other := SeriesLine{Name: "other", Values: make([]*float64, n)}
	for _, l := range lines[maxNamedSeries:] {
		other.Total += l.Total
		for i, v := range l.Values {
			if v == nil {
				continue
			}
			sum := *v
			if other.Values[i] != nil {
				sum += *other.Values[i]
			}
			other.Values[i] = &sum
		}
	}
	return append(keep, other)
}

// callerTenants are the organisations whose metrics the caller may read, as
// extra_filters; nil means every tenant (single-tenant installs).
func callerTenants(r *http.Request) []string {
	sc := scopeFrom(r.Context())
	if sc.all {
		return nil
	}
	tenants := []string{}
	for _, m := range sc.orgs {
		tenants = append(tenants, m.Org.ID)
	}
	return tenants
}
