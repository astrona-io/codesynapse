package api

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// The models section of the platform statistics (docs/PLATFORM_STATS_PLAN.md,
// phase 5): what CodeSynapse's own model calls did across every
// organisation, the local and cloud split, and what they cost. The queries
// are fixed MetricsQL with the window as the only value put in; one
// organisation is selected through extra_filters, never the query text.
//
// Cost is honest about what it does not know: every call either records a
// cost or one of llm_usage_unknown_total (no reported usage) and
// llm_pricing_missing_total (no configured prices), so a figure is
// "unknown" (nil) when no call of it has a price, "partial" when some lack
// one, and local providers have no cost at all. The provider's prompt-cache
// saving is shown apart and never added to CodeSynapse's savings.
//
// A model is named only when telemetry.ModelAllowed says the server's own
// configuration registered it; any other identity folds into "other".

// errModelsRange is the answer to a range the models section has no
// series for: VictoriaMetrics keeps 30 days.
var errModelsRange = errors.New("range must be 1h, 6h, 24h, 7d or 30d: model series are kept for 30 days")

// Model kinds: local and cloud come from the provider configuration
// (ProviderConfig.Local); unknown is a provider this process does not
// configure, such as series written under an older configuration.
const (
	modelLocal   = "local"
	modelCloud   = "cloud"
	modelUnknown = "unknown"
)

// Cost bases of a figure.
const (
	costNone     = "none"     // no calls
	costLocal    = "local"    // only local calls, which have no cost
	costEstimate = "estimate" // every billable call had a price
	costPartial  = "partial"  // some billable calls had no price; the figure leaves them out
	costUnknown  = "unknown"  // no billable call had a price
)

// ModelFigures are model calls added up: for one model, one organisation or
// the platform.
type ModelFigures struct {
	Requests     float64 `json:"requests"`
	Failures     float64 `json:"failures"`
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	// CachedTokens is the part of InputTokens the provider's prompt cache
	// served.
	CachedTokens float64 `json:"cached_tokens"`
	// CostDollars is the estimated spend of the billable (not local) calls
	// that had a price; nil when CostBasis is unknown, local or none.
	CostDollars *float64 `json:"cost_dollars"`
	CostBasis   string   `json:"cost_basis"`
	// UnknownUsageCalls and PricingMissingCalls are billable calls without
	// a cost: no reported usage, or no configured prices.
	UnknownUsageCalls   float64 `json:"unknown_usage_calls"`
	PricingMissingCalls float64 `json:"pricing_missing_calls"`
	LocalRequests       float64 `json:"local_requests"`
	CloudRequests       float64 `json:"cloud_requests"`
	UnknownRequests     float64 `json:"unknown_requests"`
	LocalTokens         float64 `json:"local_tokens"`
	CloudTokens         float64 `json:"cloud_tokens"`
	UnknownTokens       float64 `json:"unknown_tokens"`
	// LocalSharePct is the local part of the requests; nil without any.
	LocalSharePct *float64 `json:"local_share_pct"`
	// CloudBytes are request body bytes sent to cloud models.
	CloudBytes float64 `json:"cloud_bytes"`
	// ProviderCacheSavedDollars is what the provider's prompt cache took off
	// the priced calls, at the configured prices. It is the provider's
	// saving, never CodeSynapse's; nil when no cost is known.
	ProviderCacheSavedDollars *float64 `json:"provider_cache_saved_dollars"`
}

// ModelRow is one model's line. Price fields name where a cloud provider's
// prices come from.
type ModelRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Kind     string `json:"kind"`
	// Folded marks the "other" rows: models not in the configuration and
	// those past the top maxNamedValues.
	Folded bool `json:"folded"`
	ModelFigures
	Priced       bool   `json:"priced"`
	PriceSource  string `json:"price_source,omitempty"`
	PriceChecked string `json:"price_checked,omitempty"`
	PriceFeed    string `json:"price_feed,omitempty"`
	// TTFTP95Seconds (streamed answers only) and RequestP95Seconds; nil for
	// folded rows and when nothing was observed.
	TTFTP95Seconds    *float64    `json:"ttft_p95_seconds"`
	RequestP95Seconds *float64    `json:"request_p95_seconds"`
	Tasks             []ModelTask `json:"tasks"`
}

// ModelTotals are the platform's (or the selected organisation's) figures.
type ModelTotals struct {
	ModelFigures
	// EmbeddedEntities and OffloadTurns are memory work: entity names
	// embedded for recall by meaning, and conversation turns handed to
	// background extraction.
	EmbeddedEntities float64 `json:"embedded_entities"`
	OffloadTurns     float64 `json:"offload_turns"`
}

// ModelOrgRow is one organisation's line. An organisation deleted since has
// no slug and reads "deleted organisation".
type ModelOrgRow struct {
	OrgID   string `json:"org_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
	ModelFigures
}

// ModelsSection is the models section: totals, a page of organisations and
// the model rows.
type ModelsSection struct {
	StatsTable[ModelTotals, ModelOrgRow]
	Models []ModelRow `json:"models"`
}

// adminModelQueries; $r is the window. The (platform_)? twins hold calls
// made without a trusted tenant, so they count in the platform totals; an
// organisation's extra_filters leave them out.
var adminModelQueries = struct {
	requests, input, output, cached, cost, cacheSaved, usageUnknown, pricingMissing, cloudBytes string
	ttft, duration, embedded, offload                                                           string
}{
	requests:       `sum by (tenant_id, llm_provider, llm_model, task, outcome)(increase({__name__=~"(platform_)?llm_requests_total"}[$r]))`,
	input:          `sum by (tenant_id, llm_provider, llm_model, task)(increase({__name__=~"(platform_)?llm_tokens_input_total"}[$r]))`,
	output:         `sum by (tenant_id, llm_provider, llm_model, task)(increase({__name__=~"(platform_)?llm_tokens_output_total"}[$r]))`,
	cached:         `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_tokens_saved_total"}[$r]))`,
	cost:           `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_cost_estimated_dollars"}[$r]))`,
	cacheSaved:     `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_cost_saved_estimated_dollars_total"}[$r]))`,
	usageUnknown:   `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_usage_unknown_total"}[$r]))`,
	pricingMissing: `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_pricing_missing_total"}[$r]))`,
	cloudBytes:     `sum by (tenant_id, llm_provider, llm_model)(increase({__name__=~"(platform_)?cloud_bytes_sent_total"}[$r]))`,
	ttft:           `histogram_quantile(0.95, sum by (le, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_time_to_first_token_seconds_bucket"}[$r])))`,
	duration:       `histogram_quantile(0.95, sum by (le, llm_provider, llm_model)(increase({__name__=~"(platform_)?llm_request_duration_seconds_bucket"}[$r])))`,
	embedded:       `sum(increase({__name__=~"(platform_)?memory_entities_embedded_total"}[$r]))`,
	offload:        `sum(increase({__name__=~"(platform_)?memory_offload_turns_total"}[$r]))`,
}

// modelCell is one tenant's calls of one stored model identity.
type modelCell struct {
	requests, failures, input, output, cached float64
	cost, saved, usageUnknown, pricingMissing float64
	bytes                                     float64
	hasCost                                   bool
	tasks                                     map[string]*ModelTask
	tenant, provider, model, kind             string
}

// modelsData is the section before paging.
type modelsData struct {
	totals ModelTotals
	rows   []ModelOrgRow
	models []ModelRow
}

var modelSorts = map[string]func(ModelOrgRow) int{
	"requests": func(o ModelOrgRow) int { return int(o.Requests) },
	"failures": func(o ModelOrgRow) int { return int(o.Failures) },
	"tokens":   func(o ModelOrgRow) int { return int(o.InputTokens + o.OutputTokens) },
	"cost":     func(o ModelOrgRow) int { return int(math.Round(deref(o.CostDollars) * 1e4)) },
	"local":    func(o ModelOrgRow) int { return int(math.Round(deref(o.LocalSharePct) * 10)) },
}

// errNoMetrics answers 409: the section reads VictoriaMetrics only.
var errNoMetrics = errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)")

func (s *Server) handleStatsModels(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errNoMetrics)
		return
	}
	req := statsRequest(r, "models")
	if _, ok := seriesRanges[req.Range]; !ok {
		s.problem(w, http.StatusBadRequest, errModelsRange)
		return
	}
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	d, err := s.modelStats(r.Context(), req.Range, q)
	if err != nil {
		s.modelsError(w, err)
		return
	}
	sortRows(d.rows, modelSorts[r.URL.Query().Get("sort")])
	writeJSON(w, http.StatusOK, ModelsSection{StatsTable: page(r, req.Range, d.totals, d.rows, nil), Models: d.models})
}

// modelsError answers a failed models read: VictoriaMetrics errors are a
// bad gateway, the rest as for every statistics read.
func (s *Server) modelsError(w http.ResponseWriter, err error) {
	var upstream metricsError
	switch {
	case errors.Is(err, errNoMetrics):
		s.problem(w, http.StatusConflict, err)
	case errors.As(err, &upstream):
		s.problem(w, http.StatusBadGateway, upstream.err)
	default:
		s.statsError(w, err)
	}
}

// metricsError marks a failed VictoriaMetrics query.
type metricsError struct{ err error }

func (e metricsError) Error() string { return e.err.Error() }
func (e metricsError) Unwrap() error { return e.err }

// modelStats reads the window rng (one of seriesRanges) ending at q.Now.
func (s *Server) modelStats(ctx context.Context, rng string, q accounts.StatsQuery) (modelsData, error) {
	if s.cfg.Metrics == nil {
		return modelsData{}, errNoMetrics
	}
	sr, ok := seriesRanges[rng]
	if !ok {
		return modelsData{}, errModelsRange
	}
	var tenants []string // nil: every tenant
	if q.OrgID != "" {
		tenants = []string{q.OrgID}
	}
	end := q.Now.Truncate(time.Second)
	each := func(expr string, add func(map[string]string, float64)) error {
		res, err := s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(expr, "$r", rng), end, end, sr.span, tenants)
		if err != nil {
			return metricsError{err}
		}
		for _, ts := range res {
			if len(ts.Points) == 0 {
				continue
			}
			if v := ts.Points[len(ts.Points)-1].V; !math.IsNaN(v) && !math.IsInf(v, 0) {
				add(ts.Labels, v)
			}
		}
		return nil
	}
	cells := map[[3]string]*modelCell{}
	cell := func(l map[string]string) *modelCell {
		k := [3]string{l["tenant_id"], l["llm_provider"], l["llm_model"]}
		c := cells[k]
		if c == nil {
			c = &modelCell{tenant: k[0], provider: k[1], model: k[2], tasks: map[string]*ModelTask{}}
			cells[k] = c
		}
		return c
	}
	task := func(c *modelCell, name string) *ModelTask {
		name = cmp.Or(name, "other")
		t := c.tasks[name]
		if t == nil {
			t = &ModelTask{Task: name}
			c.tasks[name] = t
		}
		return t
	}
	ttft, duration := map[[2]string]float64{}, map[[2]string]float64{}
	var embedded, offload float64
	steps := []struct {
		expr string
		add  func(map[string]string, float64)
	}{
		{adminModelQueries.requests, func(l map[string]string, v float64) {
			c := cell(l)
			t := task(c, l["task"])
			c.requests += v
			t.Requests += v
			// Every outcome but ok is a failure, as in GET /api/v1/stats/models.
			if l["outcome"] != "ok" {
				c.failures += v
				t.Failures += v
			}
		}},
		{adminModelQueries.input, func(l map[string]string, v float64) {
			c := cell(l)
			c.input += v
			task(c, l["task"]).InputTokens += v
		}},
		{adminModelQueries.output, func(l map[string]string, v float64) {
			c := cell(l)
			c.output += v
			task(c, l["task"]).OutputTokens += v
		}},
		{adminModelQueries.cached, func(l map[string]string, v float64) { cell(l).cached += v }},
		{adminModelQueries.cost, func(l map[string]string, v float64) {
			c := cell(l)
			c.cost += v
			c.hasCost = true
		}},
		{adminModelQueries.cacheSaved, func(l map[string]string, v float64) { cell(l).saved += v }},
		{adminModelQueries.usageUnknown, func(l map[string]string, v float64) { cell(l).usageUnknown += v }},
		{adminModelQueries.pricingMissing, func(l map[string]string, v float64) { cell(l).pricingMissing += v }},
		{adminModelQueries.cloudBytes, func(l map[string]string, v float64) { cell(l).bytes += v }},
		{adminModelQueries.ttft, func(l map[string]string, v float64) { ttft[[2]string{l["llm_provider"], l["llm_model"]}] = v }},
		{adminModelQueries.duration, func(l map[string]string, v float64) { duration[[2]string{l["llm_provider"], l["llm_model"]}] = v }},
		{adminModelQueries.embedded, func(_ map[string]string, v float64) { embedded += v }},
		{adminModelQueries.offload, func(_ map[string]string, v float64) { offload += v }},
	}
	for _, st := range steps {
		if err := each(st.expr, st.add); err != nil {
			return modelsData{}, err
		}
	}
	all := make([]*modelCell, 0, len(cells))
	for _, k := range slices.SortedFunc(func(yield func([3]string) bool) {
		for k := range cells {
			if !yield(k) {
				return
			}
		}
	}, func(a, b [3]string) int { return cmp.Compare(strings.Join(a[:], "\xff"), strings.Join(b[:], "\xff")) }) {
		c := cells[k]
		c.kind, _ = s.providerKind(c.provider)
		all = append(all, c)
	}
	d := modelsData{totals: ModelTotals{ModelFigures: sumModelCells(all), EmbeddedEntities: math.Round(embedded), OffloadTurns: math.Round(offload)}}
	d.models = s.modelRows(all, ttft, duration)
	byOrg := map[string][]*modelCell{}
	for _, c := range all {
		if c.tenant != "" && c.tenant != telemetry.NoTenant {
			byOrg[c.tenant] = append(byOrg[c.tenant], c)
		}
	}
	rows, err := s.modelOrgRows(ctx, q.OrgID, byOrg)
	if err != nil {
		return modelsData{}, err
	}
	d.rows = rows
	return d, nil
}

// providerKind is local or cloud for a configured provider, cloud for the
// gateway's pass-through, and unknown otherwise.
func (s *Server) providerKind(provider string) (string, llm.ProviderConfig) {
	if s.cfg.LLM != nil {
		if cfg, ok := s.cfg.LLM.Config(provider); ok {
			if cfg.Local() {
				return modelLocal, cfg
			}
			return modelCloud, cfg
		}
	}
	if provider == llm.GatewayProvider {
		return modelCloud, llm.ProviderConfig{}
	}
	return modelUnknown, llm.ProviderConfig{}
}

// sumModelCells adds cells up and works out their cost basis. Local calls
// never carry a cost or count as unknown cost.
func sumModelCells(cells []*modelCell) ModelFigures {
	var f ModelFigures
	var cost, saved, billable float64
	for _, c := range cells {
		f.Requests += c.requests
		f.Failures += c.failures
		f.InputTokens += c.input
		f.OutputTokens += c.output
		f.CachedTokens += c.cached
		tokens := c.input + c.output
		switch c.kind {
		case modelLocal:
			f.LocalRequests += c.requests
			f.LocalTokens += tokens
			continue
		case modelCloud:
			f.CloudRequests += c.requests
			f.CloudTokens += tokens
			f.CloudBytes += c.bytes
		default:
			f.UnknownRequests += c.requests
			f.UnknownTokens += tokens
			f.CloudBytes += c.bytes
		}
		billable += c.requests
		f.UnknownUsageCalls += c.usageUnknown
		f.PricingMissingCalls += c.pricingMissing
		if c.hasCost {
			cost += c.cost
			saved += c.saved
		}
	}
	for _, p := range []*float64{&f.Requests, &f.Failures, &f.InputTokens, &f.OutputTokens, &f.CachedTokens, &f.UnknownUsageCalls, &f.PricingMissingCalls,
		&f.LocalRequests, &f.CloudRequests, &f.UnknownRequests, &f.LocalTokens, &f.CloudTokens, &f.UnknownTokens, &f.CloudBytes} {
		*p = math.Round(*p)
	}
	billable = math.Round(billable)
	if f.Requests > 0 {
		f.LocalSharePct = ptr(round1(100 * f.LocalRequests / f.Requests))
	}
	unknown := f.UnknownUsageCalls + f.PricingMissingCalls
	switch {
	case billable == 0 && f.LocalRequests > 0:
		f.CostBasis = costLocal
	case billable == 0:
		f.CostBasis = costNone
	case unknown >= billable:
		f.CostBasis = costUnknown
	default:
		f.CostBasis = costEstimate
		if unknown > 0 {
			f.CostBasis = costPartial
		}
		f.CostDollars, f.ProviderCacheSavedDollars = ptr(round4(cost)), ptr(round4(saved))
	}
	return f
}

// modelKey is a model row's identity.
type modelKey struct{ provider, model, kind string }

// modelRows groups cells by model: a model is named only when this
// process's configuration allowed it; the top maxNamedValues by requests
// keep their name and the rest fold into one "other" row per kind.
func (s *Server) modelRows(cells []*modelCell, ttft, duration map[[2]string]float64) []ModelRow {
	named := func(c *modelCell) modelKey {
		if telemetry.ModelAllowed(c.provider, c.model) {
			return modelKey{c.provider, c.model, c.kind}
		}
		provider := "other"
		if c.kind != modelUnknown {
			provider = c.provider // a configured provider's name is the server's own
		}
		return modelKey{provider, "other", c.kind}
	}
	groups := map[modelKey][]*modelCell{}
	for _, c := range cells {
		k := named(c)
		groups[k] = append(groups[k], c)
	}
	type group struct {
		key      modelKey
		requests float64
	}
	var order []group
	for k, cs := range groups {
		g := group{key: k}
		for _, c := range cs {
			g.requests += c.requests + c.input + c.output
		}
		order = append(order, g)
	}
	isFolded := func(k modelKey) bool { return k.model == "other" }
	slices.SortFunc(order, func(a, b group) int {
		return cmp.Or(cmp.Compare(b.requests, a.requests), cmp.Compare(a.key.provider+"/"+a.key.model, b.key.provider+"/"+b.key.model), cmp.Compare(a.key.kind, b.key.kind))
	})
	final := map[modelKey][]*modelCell{}
	var keys []modelKey
	kept := 0
	for _, g := range order {
		k := g.key
		if !isFolded(k) {
			if kept >= maxNamedValues {
				k = modelKey{"other", "other", k.kind}
			} else {
				kept++
			}
		}
		if _, ok := final[k]; !ok {
			keys = append(keys, k)
		}
		final[k] = append(final[k], groups[g.key]...)
	}
	// Folded rows come after the named ones.
	slices.SortStableFunc(keys, func(a, b modelKey) int {
		return cmp.Compare(boolInt(isFolded(a)), boolInt(isFolded(b)))
	})
	out := make([]ModelRow, 0, len(keys))
	for _, k := range keys {
		cs := final[k]
		row := ModelRow{Provider: k.provider, Model: k.model, Kind: k.kind, Folded: isFolded(k), ModelFigures: sumModelCells(cs), Tasks: []ModelTask{}}
		if row.Kind == modelCloud && k.provider != "other" {
			if _, cfg := s.providerKind(k.provider); cfg.Name != "" {
				row.Priced = cfg.PriceInputPerMTok > 0 && cfg.PriceOutputPerMTok > 0
				row.PriceSource, row.PriceChecked, row.PriceFeed = cfg.PriceSource, cfg.PriceChecked, cfg.PriceFeed
			}
		}
		if !row.Folded {
			id := [2]string{k.provider, k.model}
			if v, ok := ttft[id]; ok {
				row.TTFTP95Seconds = ptr(round3(v))
			}
			if v, ok := duration[id]; ok {
				row.RequestP95Seconds = ptr(round3(v))
			}
		}
		tasks := map[string]*ModelTask{}
		for _, c := range cs {
			for name, t := range c.tasks {
				if tasks[name] == nil {
					tasks[name] = &ModelTask{Task: name}
				}
				tasks[name].Requests += t.Requests
				tasks[name].Failures += t.Failures
				tasks[name].InputTokens += t.InputTokens
				tasks[name].OutputTokens += t.OutputTokens
			}
		}
		for _, t := range tasks {
			t.Requests, t.Failures, t.InputTokens, t.OutputTokens = math.Round(t.Requests), math.Round(t.Failures), math.Round(t.InputTokens), math.Round(t.OutputTokens)
			row.Tasks = append(row.Tasks, *t)
		}
		slices.SortFunc(row.Tasks, func(a, b ModelTask) int {
			return cmp.Or(cmp.Compare(b.Requests, a.Requests), cmp.Compare(a.Task, b.Task))
		})
		out = append(out, row)
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// modelOrgRows lists the organisations: every existing one (only orgID
// when set) by name, then deleted ones that made calls.
func (s *Server) modelOrgRows(ctx context.Context, orgID string, byOrg map[string][]*modelCell) ([]ModelOrgRow, error) {
	orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	rows := []ModelOrgRow{}
	exists := map[string]bool{}
	for _, o := range orgs {
		exists[o.ID] = true
		if orgID != "" && o.ID != orgID {
			continue
		}
		rows = append(rows, ModelOrgRow{OrgID: o.ID, Slug: o.Slug, Name: o.Name, ModelFigures: sumModelCells(byOrg[o.ID])})
	}
	slices.SortStableFunc(rows, func(a, b ModelOrgRow) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Slug, b.Slug))
	})
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range byOrg {
			if !yield(k) {
				return
			}
		}
	}) {
		if exists[id] || (orgID != "" && id != orgID) {
			continue
		}
		rows = append(rows, ModelOrgRow{OrgID: id, Name: deletedOrg, Deleted: true, ModelFigures: sumModelCells(byOrg[id])})
	}
	return rows, nil
}

// modelsExport is the models section for export: the organisation rows in
// CSV, everything in JSON.
func modelsExport(d modelsData) (body any, header []string, rows [][]string) {
	body = struct {
		Totals ModelTotals   `json:"totals"`
		Rows   []ModelOrgRow `json:"rows"`
		Models []ModelRow    `json:"models"`
	}{d.totals, d.rows, d.models}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	opt := func(p *float64) string {
		if p == nil {
			return ""
		}
		return num(*p)
	}
	header = []string{"slug", "name", "deleted", "requests", "failures", "input_tokens", "output_tokens", "cached_tokens", "cost_dollars", "cost_basis",
		"unknown_usage_calls", "pricing_missing_calls", "local_requests", "cloud_requests", "unknown_requests", "local_share_pct", "cloud_bytes", "provider_cache_saved_dollars"}
	for _, o := range d.rows {
		rows = append(rows, []string{csvSafe(o.Slug), csvSafe(o.Name), strconv.FormatBool(o.Deleted), num(o.Requests), num(o.Failures), num(o.InputTokens),
			num(o.OutputTokens), num(o.CachedTokens), opt(o.CostDollars), o.CostBasis, num(o.UnknownUsageCalls), num(o.PricingMissingCalls),
			num(o.LocalRequests), num(o.CloudRequests), num(o.UnknownRequests), opt(o.LocalSharePct), num(o.CloudBytes), opt(o.ProviderCacheSavedDollars)})
	}
	return body, header, rows
}
