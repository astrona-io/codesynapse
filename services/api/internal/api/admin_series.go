package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Cross-tenant charts for platform administrators
// (docs/PLATFORM_STATS_PLAN.md, phase 3). They are a table of their own,
// served only under /api/v1/admin/stats/series, so no organisation's
// Statistics page can name one. As in seriesPanels, the client names a panel
// and a range and the only value put into a query is a window from
// seriesRanges. Series grouped by tenant_id come back with organisation
// names; series grouped by anything else (tools, clients) show a name only
// when at least minGroupOrgs organisations contribute to it in the range.

// minGroupOrgs is the small-group rule: a breakdown that does not name an
// organisation shows a value only when this many organisations use it.
const minGroupOrgs = 3

// maxAdminBatchPanels bounds one admin batch (the plan's limit).
const maxAdminBatchPanels = 20

// deletedOrg names a tenant ID no organisation has any more.
const deletedOrg = "deleted organisation"

type adminPanel struct {
	seriesPanel
	// byTenant: the lines are named by tenant_id, which is mapped to the
	// organisation's name.
	byTenant bool
	// groups counts the organisations behind each value of the panel's by
	// label over the whole range: one instant of
	// `count by (X) (sum by (X, tenant_id) (increase(m[$w])) > 0)` with $w
	// the range itself. Values fewer than minGroupOrgs use fold into
	// "other" (counts) or are dropped (levels). Not run for one
	// organisation's drill-down, whose figures are shown unfolded.
	groups string
}

// orgsUsing counts the organisations that used each tool in the window.
func orgsUsing(metric string) string {
	return `count by (mcp_tool) (sum by (mcp_tool, tenant_id) (increase(` + metric + `[$w])) > 0)`
}

// gatewayRouteOrgs counts the organisations behind each gateway route.
const gatewayRouteOrgs = `count by (route) (sum by (route, tenant_id) (increase(gateway_request_duration_seconds_count[$w])) > 0)`

var adminPanels = map[string]adminPanel{
	"org_tool_calls": {byTenant: true, seriesPanel: seriesPanel{Title: "Tool calls by organisation", Unit: "calls", Kind: "count",
		Help:    "MCP tool and prompt calls per step, by organisation.",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(mcp_tool_calls_total[$w]))`, by: "tenant_id"}}}},
	"org_sessions": {byTenant: true, seriesPanel: seriesPanel{Title: "Agent sessions by organisation", Unit: "sessions", Kind: "count",
		Help:    "MCP sessions started per step, by organisation.",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(mcp_sessions_total[$w]))`, by: "tenant_id"}}}},
	"org_tokens": {byTenant: true, seriesPanel: seriesPanel{Title: "Tokens served by organisation", Unit: "tokens", Kind: "count",
		Help:    "Estimated tokens MCP tools returned to agents, by organisation.",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(mcp_response_tokens_total[$w]))`, by: "tenant_id"}}}},
	"sessions": {seriesPanel: seriesPanel{Title: "Agent sessions", Unit: "sessions", Kind: "count",
		Help:    "MCP sessions started per step.",
		queries: []seriesQuery{{name: "sessions", expr: `sum(increase(mcp_sessions_total[$w]))`}}}},
	"mcp_calls": {groups: orgsUsing("mcp_tool_calls_total"), seriesPanel: seriesPanel{Title: "Tool calls", Unit: "calls", Kind: "count",
		Help:    "MCP tool and prompt calls per step, by tool. A tool fewer than three organisations used in the range is counted under other.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total[$w]))`, by: "mcp_tool"}}}},
	"mcp_errors": {groups: orgsUsing(`mcp_tool_calls_total{outcome="error"}`), seriesPanel: seriesPanel{Title: "Failed tool calls", Unit: "calls", Kind: "count",
		Help:    "Calls that ran and returned an error, by tool. A tool fewer than three organisations saw fail in the range is counted under other.",
		Empty:   "No tool call failed in this window.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total{outcome="error"}[$w]))`, by: "mcp_tool"}}}},
	"mcp_tokens": {groups: orgsUsing("mcp_response_tokens_total"), seriesPanel: seriesPanel{Title: "Tokens served to agents", Unit: "tokens", Kind: "count",
		Help:    "Estimated tokens MCP tools returned to agents, by tool. A tool fewer than three organisations used in the range is counted under other.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_response_tokens_total[$w]))`, by: "mcp_tool"}}}},
	"local_answers": {groups: orgsUsing(`mcp_local_answers_total{outcome="recorded"}`), seriesPanel: seriesPanel{Title: "Answered on the developer's machine", Unit: "calls", Kind: "count",
		Help:    "Calls the Claude Code bridge answered locally, by tool. Compare with tool calls for the share answered by the server. A tool fewer than three organisations used in the range is counted under other.",
		Empty:   "No call was answered locally in this window.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_local_answers_total{outcome="recorded"}[$w]))`, by: "mcp_tool"}}}},
	"tool_latency": {seriesPanel: seriesPanel{Title: "Tool latency", Unit: "seconds", Kind: "gauge",
		Help: "Median and 95th percentile time to answer a tool call, every organisation.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
		}}},
	"tool_latency_by_tool": {groups: orgsUsing("mcp_tool_duration_seconds_count"), seriesPanel: seriesPanel{Title: "Slowest tools", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time to answer, for the tools at least three organisations used in the range.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, mcp_tool) (increase(mcp_tool_duration_seconds_bucket[$w])))`, by: "mcp_tool"}}}},
	"api_routes": {seriesPanel: seriesPanel{Title: "API requests by route", Unit: "requests", Kind: "count",
		Help:    "API and worker requests per step by route pattern, signed in or not.",
		queries: []seriesQuery{{expr: `sum by (route) (increase({__name__=~"(platform_)?http_requests_total"}[$w]))`, by: "route"}}}},
	"api_failures": {seriesPanel: seriesPanel{Title: "API server errors by route", Unit: "requests", Kind: "count",
		Help:    "API requests that answered 5xx, by route pattern.",
		Empty:   "No request failed on the server in this window.",
		queries: []seriesQuery{{expr: `sum by (route) (increase({__name__=~"(platform_)?http_requests_total",code=~"5.*"}[$w]))`, by: "route"}}}},
	"api_route_latency": {seriesPanel: seriesPanel{Title: "API latency by route", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile request time by route pattern.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, route) (increase({__name__=~"(platform_)?http_request_duration_seconds_bucket"}[$w])))`, by: "route"}}}},
	"gateway_requests": {seriesPanel: seriesPanel{Title: "Gateway requests", Unit: "requests", Kind: "count",
		Help:    "Gateway requests by route (local, cloud, rejected) and status class.",
		Empty:   "No gateway request in this window.",
		queries: []seriesQuery{{expr: `sum by (series) (label_join(increase(gateway_requests_total[$w]), "series", " ", "route", "class"))`, by: "series"}}}},
	"gateway_latency": {groups: gatewayRouteOrgs, seriesPanel: seriesPanel{Title: "Gateway response time (p95)", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time to answer a gateway request, by route, every organisation.",
		Empty:   "No gateway request in this window.",
		queries: []seriesQuery{{expr: gatewayP95, by: "route"}}}},
	"gateway_latency_avg": {groups: gatewayRouteOrgs, seriesPanel: seriesPanel{Title: "Gateway response time (average)", Unit: "seconds", Kind: "gauge",
		Help:    "Average time to answer a gateway request, by route, every organisation.",
		Empty:   "No gateway request in this window.",
		queries: []seriesQuery{{expr: gatewayAvg, by: "route"}}}},
	"gateway_fallthrough": {groups: `count by (reason) (sum by (reason, tenant_id) (increase(gateway_fallthrough_total[$w])) > 0)`, seriesPanel: seriesPanel{Title: "Requests the local model did not answer", Unit: "requests", Kind: "count",
		Help:    "Gateway requests the local model did not answer, by reason, every organisation. queue_timeout and answer_timeout mean it was too slow.",
		Empty:   "Every request offered to the local model was answered in this window.",
		queries: []seriesQuery{{expr: gatewayReasons, by: "reason"}}}},
	"llm_cost": {seriesPanel: seriesPanel{Title: "AI model cost (estimate)", Unit: "USD", Kind: "count",
		Help:    "Estimated from configured input, output and cache prices, every organisation. Calls with missing usage or prices are excluded.",
		queries: []seriesQuery{{expr: `sum by (llm_model) (increase(llm_cost_estimated_dollars[$w]))`, by: "llm_model"}}}},
	"audit_writes": {seriesPanel: seriesPanel{Title: "Audit log writes", Unit: "entries", Kind: "count",
		Help:    "Audit entries written by the API, the MCP service and background work, and writes that failed. A failed write withholds the answer it was for.",
		Empty:   "No audit entry was written in this window.",
		queries: []seriesQuery{{expr: `sum by (series) (label_join(increase(codesynapse_audit_writes_total[$w]), "series", " ", "source", "outcome"))`, by: "series"}}}},
	"audit_seals": {seriesPanel: seriesPanel{Title: "Audit archive sealing", Unit: "runs", Kind: "count",
		Help:    "Daily sealing runs by outcome. no_archive means sealed segments exist but no archive is configured, so pruning waits.",
		Empty:   "No sealing run in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(platform_codesynapse_audit_seals_total[$w]))`, by: "outcome"}}}},
	"db_latency": {seriesPanel: seriesPanel{Title: "Database latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile round trip by database.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, db_system) (increase(db_query_duration_seconds_bucket[$w])))`, by: "db_system"}}}},
}

// adminPanelNames are the requested panels: trimmed, without repeats.
func adminPanelNames(list string) []string {
	var names []string
	for n := range strings.SplitSeq(list, ",") {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

func (s *Server) handleAdminSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, rng := q.Get("panel"), cmp.Or(q.Get("range"), "24h")
	if _, ok := adminPanels[name]; !ok {
		s.problem(w, http.StatusNotFound, errors.New("unknown panel"))
		return
	}
	if _, ok := seriesRanges[rng]; !ok {
		s.problem(w, http.StatusBadRequest, errors.New("range must be 1h, 6h, 24h, 7d or 30d"))
		return
	}
	sq, ok := s.platformView(w, r, accounts.StatsRequest{Section: "series", Panel: name, Range: rng, OrgID: q.Get("org")})
	if !ok {
		return
	}
	s.writeAdminPanel(r.Context(), w, name, rng, sq.OrgID)
}

func (s *Server) handleAdminSeriesBatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	names, rng := adminPanelNames(q.Get("panels")), cmp.Or(q.Get("range"), "24h")
	if len(names) == 0 || len(names) > maxAdminBatchPanels {
		s.problem(w, http.StatusBadRequest, errors.New("panels must name 1 to 20 panels, comma separated"))
		return
	}
	if _, ok := seriesRanges[rng]; !ok {
		s.problem(w, http.StatusBadRequest, errors.New("range must be 1h, 6h, 24h, 7d or 30d"))
		return
	}
	// Only known names reach the audit target; unknown ones answer 404
	// each, as on their own.
	var known []string
	for _, n := range names {
		if _, ok := adminPanels[n]; ok {
			known = append(known, n)
		}
	}
	sq, ok := s.platformView(w, r, accounts.StatsRequest{Section: "series", Panel: strings.Join(known, ","), Range: rng, OrgID: q.Get("org")})
	if !ok {
		return
	}
	items := make([]SeriesBatchItem, len(names))
	_ = forEach(r.Context(), len(names), func(ctx context.Context, i int) error { //nolint:errcheck // each item carries its own status
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		if _, ok := adminPanels[names[i]]; ok {
			s.writeAdminPanel(ctx, rec, names[i], rng, sq.OrgID)
		} else {
			s.problem(rec, http.StatusNotFound, errors.New("unknown panel"))
		}
		items[i] = SeriesBatchItem{Status: rec.status, Body: json.RawMessage(bytes.TrimSpace(rec.body.Bytes()))}
		return nil
	})
	out := make(map[string]SeriesBatchItem, len(names))
	for i, n := range names {
		out[n] = items[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"panels": out})
}

// writeAdminPanel answers one authorised admin panel. orgID, already
// checked against PostgreSQL, limits it to one organisation.
func (s *Server) writeAdminPanel(ctx context.Context, w http.ResponseWriter, name, rng, orgID string) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)"))
		return
	}
	out, err := s.adminPanelData(ctx, name, rng, orgID)
	if err != nil {
		s.problem(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminPanelData(ctx context.Context, name, rng, orgID string) (SeriesPanel, error) {
	p := adminPanels[name]
	var tenants []string // nil: every tenant
	if orgID != "" {
		tenants = []string{orgID}
	}
	out, err := s.runPanel(ctx, name, rng, p.seriesPanel, tenants, "")
	if err != nil {
		return SeriesPanel{}, err
	}
	if p.byTenant {
		names, err := s.orgNames(ctx)
		if err != nil {
			return SeriesPanel{}, err
		}
		for i := range out.Series {
			out.Series[i].Name = tenantName(names, out.Series[i].Name)
		}
		out.Series = mergeLines(out.Series, p.Kind)
	}
	var small *SeriesLine
	if p.groups != "" && orgID == "" {
		used, err := s.orgsPerValue(ctx, p, rng)
		if err != nil {
			return SeriesPanel{}, err
		}
		out.Series, small = foldSmallGroups(out.Series, used, p.Kind, len(out.Times))
	}
	out.Series = addOther(foldSeries(out.Series, p.Kind, len(out.Times)), small)
	return out, nil
}

// orgsPerValue runs a panel's groups query once over the whole range: how
// many organisations stand behind each value of its by label.
func (s *Server) orgsPerValue(ctx context.Context, p adminPanel, rng string) (map[string]int, error) {
	sr := seriesRanges[rng]
	at := time.Now().Truncate(time.Second)
	// The range names are MetricsQL durations themselves (1h ... 30d).
	res, err := s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(p.groups, "$w", rng), at, at, sr.span, nil)
	if err != nil {
		return nil, err
	}
	by := p.queries[0].by
	used := map[string]int{}
	for _, ts := range res {
		for _, pt := range ts.Points {
			if !math.IsNaN(pt.V) && !math.IsInf(pt.V, 0) {
				used[ts.Labels[by]] = max(used[ts.Labels[by]], int(pt.V))
			}
		}
	}
	return used, nil
}

// foldSmallGroups takes out the lines fewer than minGroupOrgs organisations
// stand behind: summed into one "other" line for counts, dropped for levels,
// which cannot be summed.
func foldSmallGroups(lines []SeriesLine, used map[string]int, kind string, n int) ([]SeriesLine, *SeriesLine) {
	var keep, small []SeriesLine
	for _, l := range lines {
		if used[l.Name] >= minGroupOrgs {
			keep = append(keep, l)
		} else {
			small = append(small, l)
		}
	}
	if keep == nil {
		keep = []SeriesLine{}
	}
	if len(small) == 0 || kind != "count" {
		return keep, nil
	}
	other := sumLines("other", small, n)
	return keep, &other
}

// addOther adds the small groups to the top-N fold's "other" line.
func addOther(lines []SeriesLine, small *SeriesLine) []SeriesLine {
	if small == nil {
		return lines
	}
	for i, l := range lines {
		if l.Name == "other" {
			lines[i] = sumLines("other", []SeriesLine{l, *small}, len(l.Values))
			return lines
		}
	}
	return append(lines, *small)
}

// sumLines adds lines step by step; a step stays empty only when every
// line is empty there.
func sumLines(name string, lines []SeriesLine, n int) SeriesLine {
	out := SeriesLine{Name: name, Values: make([]*float64, n)}
	for _, l := range lines {
		out.Total += l.Total
		for i, v := range l.Values {
			if v == nil || i >= n {
				continue
			}
			sum := *v
			if out.Values[i] != nil {
				sum += *out.Values[i]
			}
			out.Values[i] = &sum
		}
	}
	return out
}

// mergeLines joins lines that ended up with the same name (deleted
// organisations): summed for counts, the higher level for gauges.
func mergeLines(lines []SeriesLine, kind string) []SeriesLine {
	var out []SeriesLine
	at := map[string]int{}
	for _, l := range lines {
		i, ok := at[l.Name]
		if !ok {
			at[l.Name] = len(out)
			out = append(out, l)
			continue
		}
		if kind == "count" {
			out[i] = sumLines(l.Name, []SeriesLine{out[i], l}, len(l.Values))
			continue
		}
		m := out[i]
		m.Values = slices.Clone(m.Values)
		for j, v := range l.Values {
			if v != nil && (m.Values[j] == nil || *v > *m.Values[j]) {
				m.Values[j] = v
			}
		}
		m.Total = math.Max(m.Total, l.Total)
		out[i] = m
	}
	if out == nil {
		out = []SeriesLine{}
	}
	return out
}

// orgNames maps organisation IDs to the names shown in admin charts and
// tables. Two organisations with the same name are told apart by slug.
func (s *Server) orgNames(ctx context.Context) (map[string]string, error) {
	orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for _, o := range orgs {
		seen[o.Name]++
	}
	names := make(map[string]string, len(orgs))
	for _, o := range orgs {
		names[o.ID] = o.Name
		if seen[o.Name] > 1 || o.Name == "" || o.Name == deletedOrg {
			names[o.ID] = o.Name + " (" + o.Slug + ")"
		}
	}
	return names, nil
}

// tenantName is the name of a tenant_id label value; series recorded
// without a tenant read "none".
func tenantName(names map[string]string, id string) string {
	if id == "none" || id == "" {
		return "no organisation"
	}
	if n, ok := names[id]; ok {
		return n
	}
	return deletedOrg
}
