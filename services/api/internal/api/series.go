package api

import (
	"cmp"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
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
	Title, Unit, Help string
	// Kind is "count" (per step) or "gauge" (latency, a level per step).
	Kind     string
	queries  []seriesQuery
	platform bool // cross-tenant: platform admins only
}

var seriesPanels = map[string]seriesPanel{
	"tool_calls": {Title: "Tool calls", Unit: "calls", Kind: "count",
		Help:    "MCP tool and prompt calls per step, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total[$w]))`, by: "mcp_tool"}}},
	"tool_errors": {Title: "Failed tool calls", Unit: "calls", Kind: "count",
		Help:    "Calls that returned an error, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total{outcome="error"}[$w]))`, by: "mcp_tool"}}},
	"sessions": {Title: "Agent sessions", Unit: "sessions", Kind: "count",
		Help:    "MCP sessions started per step.",
		queries: []seriesQuery{{name: "sessions", expr: `sum(increase(mcp_sessions_total[$w]))`}}},
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
		Help: "Tokens CodeSynapse's own AI tasks sent and received; saved = served from the provider's prompt cache.",
		queries: []seriesQuery{
			{name: "input", expr: `sum(increase(llm_tokens_input_total[$w]))`},
			{name: "output", expr: `sum(increase(llm_tokens_output_total[$w]))`},
			{name: "saved", expr: `sum(increase(llm_tokens_saved_total[$w]))`},
		}},
	"llm_cost": {Title: "AI model cost (estimate)", Unit: "USD", Kind: "count",
		Help:    "From the token prices in the AI model settings; models without prices count as free.",
		queries: []seriesQuery{{expr: `sum by (llm_model) (increase(llm_cost_estimated_dollars[$w]))`, by: "llm_model"}}},
	"llm_ttft": {Title: "AI time to first token", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile wait for the first streamed token, by model.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, llm_model) (increase(llm_time_to_first_token_seconds_bucket[$w])))`, by: "llm_model"}}},
	"db_latency": {Title: "Database latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile round trip by database.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, db_system) (increase(db_query_duration_seconds_bucket[$w])))`, by: "db_system"}}},
	"db_rows": {Title: "Graph rows and vector matches", Unit: "rows", Kind: "count",
		Help: "Rows graph reads returned and vectors vector searches matched.",
		queries: []seriesQuery{
			{expr: `sum by (db_system) (increase(db_graph_nodes_traversed[$w]))`, by: "db_system"},
			{name: "vector matches", expr: `sum(increase(db_vector_matches_count[$w]))`},
		}},
	"memory": {Title: "Agent memory", Unit: "events", Kind: "count",
		Help: "Facts saved (by agents or extracted from offloaded turns), turns offloaded and recalls.",
		queries: []seriesQuery{
			{name: "facts saved", expr: `sum(increase(memory_facts_saved_total[$w]))`},
			{name: "turns offloaded", expr: `sum(increase(memory_offload_turns_total[$w]))`},
			{name: "recalls", expr: `sum(increase(memory_recall_total[$w]))`},
		}},
	"tenants": {Title: "Tool calls by organisation", Unit: "calls", Kind: "count", platform: true,
		Help:    "Cross-tenant activity (platform admins only).",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(mcp_tool_calls_total[$w]))`, by: "tenant_id"}}},
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
	if !sc.all && !caller.User.PlatformAdmin {
		if p.platform {
			s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
			return
		}
		tenants = []string{}
		for _, m := range sc.orgs {
			tenants = append(tenants, m.Org.ID)
		}
	}
	// The last step is the current one, still filling: recent activity
	// shows at once instead of when the step completes.
	end := time.Now().Truncate(sr.step).Add(sr.step)
	start := end.Add(-sr.span)
	out := SeriesPanel{Panel: name, Title: p.Title, Unit: p.Unit, Kind: p.Kind, Help: p.Help, Range: rng, Step: int(sr.step / time.Second), Series: []SeriesLine{}}
	for t := start.Add(sr.step); !t.After(end); t = t.Add(sr.step) {
		out.Times = append(out.Times, t.Unix())
	}
	index := make(map[int64]int, len(out.Times))
	for i, t := range out.Times {
		index[t] = i
	}
	for _, q := range p.queries {
		res, err := s.cfg.Metrics.QueryRange(r.Context(), strings.ReplaceAll(q.expr, "$w", sr.window), start.Add(sr.step), end, sr.step, tenants)
		if err != nil {
			s.problem(w, http.StatusBadGateway, err)
			return
		}
		for _, ts := range res {
			line := SeriesLine{Name: cmp.Or(q.name, ts.Labels[q.by], "none"), Values: make([]*float64, len(out.Times))}
			for _, pt := range ts.Points {
				if i, ok := index[pt.T]; ok && !math.IsInf(pt.V, 0) {
					v := round3(pt.V)
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

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
