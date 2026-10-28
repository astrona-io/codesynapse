package api

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Per-model statistics over one window, for the model table and charts:
// what each model was asked to do, how much it read and wrote, how fast,
// and how full its context window got. Like the series panels, the queries
// are fixed MetricsQL with the window as the only value put in, and tenant
// scope travels as extra_filters.

// ModelTask is one task's share of a model's work.
type ModelTask struct {
	Task         string  `json:"task"`
	Requests     float64 `json:"requests"`
	Failures     float64 `json:"failures"`
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
}

// ModelStat is one configured model's work in the window.
type ModelStat struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	Local         bool    `json:"local"`
	Configured    bool    `json:"configured"`
	ContextWindow int     `json:"context_window,omitempty"`
	Requests      float64 `json:"requests"`
	Failures      float64 `json:"failures"`
	InputTokens   float64 `json:"input_tokens"`
	OutputTokens  float64 `json:"output_tokens"`
	// CachedTokens is the part of InputTokens served from the prompt cache.
	CachedTokens float64 `json:"cached_tokens"`
	// AvgPromptTokens is input tokens per request.
	AvgPromptTokens float64 `json:"avg_prompt_tokens"`
	// Read and write speed in tokens per second, from streamed answers
	// only; nil when there were none.
	ReadTPS  *float64 `json:"read_tokens_per_second"`
	WriteTPS *float64 `json:"write_tokens_per_second"`
	// ContextP50 and ContextP95 are shares (0 to 1) of the context window
	// a request used; nil without a configured window.
	ContextP50 *float64    `json:"context_p50"`
	ContextP95 *float64    `json:"context_p95"`
	Tasks      []ModelTask `json:"tasks"`
	// CostDollars is the estimated spend from reported usage and the
	// configured prices; nil when the model's prices are not configured.
	CostDollars *float64 `json:"cost_dollars"`
	// CompressedBytes is the prompt text a local model removed before the
	// requests went to this model.
	CompressedBytes float64 `json:"compressed_bytes"`
}

// ModelStats is the answer of GET /api/v1/stats/models.
type ModelStats struct {
	Range  string      `json:"range"`
	Models []ModelStat `json:"models"`
	// Reference prices a cloud model the operator configured, so work done
	// locally can be valued at what that model would have charged; nil when
	// no cloud provider has prices, and then no cost is estimated.
	Reference *PriceReference `json:"reference_price,omitempty"`
	// Spent is the estimated cloud spend in the window and CompressionSaved
	// the dollars of input the prompt compression kept off cloud models
	// (about bytesPerToken bytes a token, at each model's input price).
	// Each is nil when a model that did the work has no configured price:
	// unknown, never zero.
	Spent            *float64 `json:"spent_dollars"`
	CompressionSaved *float64 `json:"compression_saved_dollars"`
	// CompressionEnabled says whether prompts of CodeSynapse's own AI tasks
	// are shortened before they go to a cloud model; CompressionOff says why
	// not ("disabled", "no_local_model", "no_ai"). Gateway traffic is never
	// shortened.
	CompressionEnabled bool   `json:"compression_enabled"`
	CompressionOff     string `json:"compression_off,omitempty"`
	// Gateway counts the model requests agents sent through the gateway.
	Gateway GatewayStat `json:"gateway"`
}

// GatewayStat is the gateway's traffic in the window. Routes: local (the
// local model answered), cloud (passed on), rejected (refused before any
// model).
type GatewayStat struct {
	Requests      float64 `json:"requests"`
	Local         float64 `json:"local"`
	Cloud         float64 `json:"cloud"`
	Rejected      float64 `json:"rejected"`
	Failed        float64 `json:"failed"`
	LocalSharePct float64 `json:"local_share_pct"`
	BytesIn       float64 `json:"bytes_in"`
	BytesOut      float64 `json:"bytes_out"`
	// LocalInputTokens and LocalOutputTokens are what the local model
	// reported for the requests it answered.
	LocalInputTokens  float64 `json:"local_input_tokens"`
	LocalOutputTokens float64 `json:"local_output_tokens"`
	// LocalUnmetered counts local answers whose server reported no usage.
	LocalUnmetered float64 `json:"local_unmetered"`
	// SavedDollars is what the reference cloud model would have charged for
	// the locally answered tokens; nil when any local answer was unmetered
	// (unknown, never zero).
	SavedDollars *float64 `json:"saved_dollars"`
}

// bytesPerToken turns compressed prompt bytes into a token estimate.
const bytesPerToken = 4

// PriceReference is one configured cloud provider's prices, US dollars per
// million tokens.
type PriceReference struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// CachedInputPerMTok prices cache reads; zero means the input price.
	CachedInputPerMTok float64 `json:"cached_input_per_mtok"`
	// Estimate marks the built-in fallback used when no cloud provider has
	// prices, so the page can say it is a rough figure.
	Estimate bool `json:"estimate,omitempty"`
	// Source and Checked say where the prices come from and when they
	// were read (price_source and price_checked, or the price feed).
	Source  string `json:"source,omitempty"`
	Checked string `json:"checked,omitempty"`
}

// defaultReference values saved work when no cloud provider has prices: an
// Opus-class list price (US dollars per million tokens), so the page can show
// a rough dollar figure. It is marked Estimate and replaced by any
// configured provider's prices.
var defaultReference = PriceReference{
	Provider: "built-in", Model: "Claude Opus 5.5 (list price estimate)",
	InputPerMTok: 5, OutputPerMTok: 25, CachedInputPerMTok: 0.5, Estimate: true,
}

// modelQueries; $r is the window.
var modelQueries = struct{ requests, input, output, cached, readS, writeS, readT, writeT, p50, p95, cost, compressed string }{
	cost:       `sum by (llm_provider, llm_model)(increase(llm_cost_estimated_dollars[$r]))`,
	compressed: `sum by (llm_provider, llm_model)(increase(llm_compressed_bytes_total[$r]))`,
	requests:   `sum by (llm_provider, llm_model, task, outcome)(increase(llm_requests_total[$r]))`,
	input:      `sum by (llm_provider, llm_model, task)(increase(llm_tokens_input_total[$r]))`,
	output:     `sum by (llm_provider, llm_model, task)(increase(llm_tokens_output_total[$r]))`,
	cached:     `sum by (llm_provider, llm_model)(increase(llm_tokens_saved_total[$r]))`,
	readS:      `sum by (llm_provider, llm_model)(increase(llm_read_seconds_total[$r]))`,
	writeS:     `sum by (llm_provider, llm_model)(increase(llm_write_seconds_total[$r]))`,
	readT:      `sum by (llm_provider, llm_model)(increase(llm_read_tokens_total[$r]))`,
	writeT:     `sum by (llm_provider, llm_model)(increase(llm_write_tokens_total[$r]))`,
	p50:        `histogram_quantile(0.5, sum by (le, llm_provider, llm_model)(increase(llm_context_used_ratio_bucket[$r])))`,
	p95:        `histogram_quantile(0.95, sum by (le, llm_provider, llm_model)(increase(llm_context_used_ratio_bucket[$r])))`,
}

// gatewayQueries; $r is the window.
//
//nolint:gosec // metric names, not credentials
var gatewayQueries = struct{ requests, bytes, tokens, unmetered string }{
	requests:  `sum by (route, class)(increase(gateway_requests_total[$r]))`,
	bytes:     `sum by (route, direction)(increase(gateway_bytes_total[$r]))`,
	tokens:    `sum by (kind)(increase(gateway_local_tokens_total[$r]))`,
	unmetered: `sum(increase(gateway_local_unmetered_total[$r]))`,
}

func (s *Server) handleModelStats(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)"))
		return
	}
	rng := cmp.Or(r.URL.Query().Get("range"), "7d")
	sr, ok := seriesRanges[rng]
	if !ok {
		s.problem(w, http.StatusBadRequest, errors.New("range must be 1h, 6h, 24h, 7d or 30d"))
		return
	}
	tenants := callerTenants(r)
	end := time.Now().Truncate(time.Second)
	ctx := r.Context()
	query := func(ctx context.Context, expr string) ([]telemetry.Series, error) {
		return s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(expr, "$r", rng), end, end, sr.span, tenants)
	}
	models := map[[2]string]*ModelStat{}
	stat := func(l map[string]string) *ModelStat {
		k := [2]string{l["llm_provider"], l["llm_model"]}
		m := models[k]
		if m == nil {
			m = &ModelStat{Provider: k[0], Model: k[1], Tasks: []ModelTask{}}
			models[k] = m
		}
		return m
	}
	task := func(m *ModelStat, name string) *ModelTask {
		name = cmp.Or(name, "other")
		for i := range m.Tasks {
			if m.Tasks[i].Task == name {
				return &m.Tasks[i]
			}
		}
		m.Tasks = append(m.Tasks, ModelTask{Task: name})
		return &m.Tasks[len(m.Tasks)-1]
	}
	last := func(ts telemetry.Series) (float64, bool) {
		if len(ts.Points) == 0 {
			return 0, false
		}
		v := ts.Points[len(ts.Points)-1].V
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	// each runs expr and hands every finite value to add.
	each := func(expr string, add func(map[string]string, float64)) error {
		res, err := query(ctx, expr)
		if err != nil {
			return err
		}
		for _, ts := range res {
			if v, ok := last(ts); ok {
				add(ts.Labels, v)
			}
		}
		return nil
	}
	speed := map[[2]string]*[4]float64{} // read s, write s, read tokens, write tokens
	speedOf := func(l map[string]string) *[4]float64 {
		k := [2]string{l["llm_provider"], l["llm_model"]}
		if speed[k] == nil {
			speed[k] = &[4]float64{}
		}
		return speed[k]
	}
	steps := []struct {
		expr string
		add  func(map[string]string, float64)
	}{
		{modelQueries.requests, func(l map[string]string, v float64) {
			m := stat(l)
			t := task(m, l["task"])
			m.Requests += v
			t.Requests += v
			// Every outcome but ok is a failure: error, timeout, rate_limited
			// and refused (docs/TELEMETRY_PLAN.md, phase 5).
			if l["outcome"] != "ok" {
				m.Failures += v
				t.Failures += v
			}
		}},
		{modelQueries.input, func(l map[string]string, v float64) {
			m := stat(l)
			m.InputTokens += v
			task(m, l["task"]).InputTokens += v
		}},
		{modelQueries.output, func(l map[string]string, v float64) {
			m := stat(l)
			m.OutputTokens += v
			task(m, l["task"]).OutputTokens += v
		}},
		{modelQueries.cached, func(l map[string]string, v float64) { stat(l).CachedTokens += v }},
		{modelQueries.cost, func(l map[string]string, v float64) {
			m := stat(l)
			m.CostDollars = ptr(round4(v) + deref(m.CostDollars))
		}},
		{modelQueries.compressed, func(l map[string]string, v float64) { stat(l).CompressedBytes += v }},
		{modelQueries.readS, func(l map[string]string, v float64) { speedOf(l)[0] += v }},
		{modelQueries.writeS, func(l map[string]string, v float64) { speedOf(l)[1] += v }},
		{modelQueries.readT, func(l map[string]string, v float64) { speedOf(l)[2] += v }},
		{modelQueries.writeT, func(l map[string]string, v float64) { speedOf(l)[3] += v }},
		{modelQueries.p50, func(l map[string]string, v float64) { stat(l).ContextP50 = ptr(round3(v)) }},
		{modelQueries.p95, func(l map[string]string, v float64) { stat(l).ContextP95 = ptr(round3(v)) }},
	}
	var gw GatewayStat
	steps = append(steps, []struct {
		expr string
		add  func(map[string]string, float64)
	}{
		{gatewayQueries.requests, func(l map[string]string, v float64) {
			gw.Requests += v
			switch l["route"] {
			case "local":
				gw.Local += v
			case "cloud":
				gw.Cloud += v
			default:
				gw.Rejected += v
			}
			if l["class"] == "4xx" || l["class"] == "5xx" {
				gw.Failed += v
			}
		}},
		{gatewayQueries.bytes, func(l map[string]string, v float64) {
			if l["direction"] == "in" {
				gw.BytesIn += v
			} else {
				gw.BytesOut += v
			}
		}},
		{gatewayQueries.tokens, func(l map[string]string, v float64) {
			if l["kind"] == "input" {
				gw.LocalInputTokens += v
			} else {
				gw.LocalOutputTokens += v
			}
		}},
		{gatewayQueries.unmetered, func(_ map[string]string, v float64) { gw.LocalUnmetered += v }},
	}...)
	for _, st := range steps {
		if err := each(st.expr, st.add); err != nil {
			s.problem(w, http.StatusBadGateway, err)
			return
		}
	}
	for k, sp := range speed {
		m := models[k]
		if m == nil {
			continue
		}
		if sp[0] > 0 && sp[2] > 0 {
			m.ReadTPS = ptr(round1(sp[2] / sp[0]))
		}
		if sp[1] > 0 && sp[3] > 0 {
			m.WriteTPS = ptr(round1(sp[3] / sp[1]))
		}
	}
	out := ModelStats{Range: rng, Models: []ModelStat{}}
	if s.cfg.LLM != nil {
		for _, p := range s.cfg.LLM.Configs() {
			if !p.Local() && p.PriceInputPerMTok > 0 && p.PriceOutputPerMTok > 0 {
				out.Reference = &PriceReference{Provider: p.Name, Model: p.Model, InputPerMTok: p.PriceInputPerMTok, OutputPerMTok: p.PriceOutputPerMTok, CachedInputPerMTok: p.PriceCachedInputPerMTok, Source: p.PriceSource, Checked: p.PriceChecked}
				break
			}
		}
	}
	if out.Reference == nil {
		out.Reference = &defaultReference
	}
	for _, m := range models {
		if m.Requests == 0 && m.InputTokens == 0 {
			continue
		}
		if s.cfg.LLM != nil {
			if cfg, ok := s.cfg.LLM.Config(m.Provider); ok {
				m.Configured, m.Local, m.ContextWindow = true, cfg.Local(), cfg.ContextWindow
			}
		}
		if m.Requests > 0 {
			m.AvgPromptTokens = math.Round(m.InputTokens / m.Requests)
		}
		slices.SortFunc(m.Tasks, func(a, b ModelTask) int { return cmp.Compare(b.Requests, a.Requests) })
		out.Models = append(out.Models, *m)
	}
	out.Spent, out.CompressionSaved = s.moneyTotals(out.Models)
	out.CompressionEnabled, out.CompressionOff = compressionState(s.cfg.LLM)
	for _, f := range []*float64{&gw.Requests, &gw.Local, &gw.Cloud, &gw.Rejected, &gw.Failed, &gw.LocalUnmetered, &gw.LocalInputTokens, &gw.LocalOutputTokens, &gw.BytesIn, &gw.BytesOut} {
		*f = math.Round(*f)
	}
	if gw.Requests > 0 {
		gw.LocalSharePct = round1(gw.Local / gw.Requests * 100)
	}
	// The requested Claude model is client text and not a metric label, so
	// the local tokens are priced at the reference model.
	if gw.LocalUnmetered == 0 {
		gw.SavedDollars = ptr(round4(gw.LocalInputTokens*out.Reference.InputPerMTok/1e6 + gw.LocalOutputTokens*out.Reference.OutputPerMTok/1e6))
	}
	out.Gateway = gw
	slices.SortFunc(out.Models, func(a, b ModelStat) int {
		return cmp.Or(cmp.Compare(b.InputTokens+b.OutputTokens, a.InputTokens+a.OutputTokens), cmp.Compare(a.Provider+a.Model, b.Provider+b.Model))
	})
	writeJSON(w, http.StatusOK, out)
}

func ptr(v float64) *float64 { return &v }

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// compressionState says whether prompt shortening runs and, when it does
// not, why: "no_ai" without any AI configuration, otherwise the registry's
// reason.
func compressionState(reg *llm.Registry) (bool, string) {
	if reg == nil {
		return false, "no_ai"
	}
	if st := reg.CompressionStatus(); st != llm.CompressionOn {
		return false, st
	}
	return true, ""
}

// moneyTotals sums the cloud models' spend and the dollars compression saved
// on them. A cloud model that did work without a configured price makes the
// matching total unknown (nil) instead of leaving it out.
func (s *Server) moneyTotals(models []ModelStat) (spent, compression *float64) {
	var sp, cs float64
	spentKnown, compKnown := true, true
	for _, m := range models {
		if m.Local {
			continue
		}
		priced := false
		var inPrice float64
		if s.cfg.LLM != nil {
			if cfg, ok := s.cfg.LLM.Config(m.Provider); ok && cfg.PriceInputPerMTok > 0 && cfg.PriceOutputPerMTok > 0 {
				priced, inPrice = true, cfg.PriceInputPerMTok
			}
		}
		if m.InputTokens+m.OutputTokens > 0 {
			if m.CostDollars == nil {
				spentKnown = false
			} else {
				sp += *m.CostDollars
			}
		}
		if m.CompressedBytes > 0 {
			if !priced {
				compKnown = false
			} else {
				cs += m.CompressedBytes / bytesPerToken * inPrice / 1e6
			}
		}
	}
	if spentKnown {
		spent = ptr(round4(sp))
	}
	if compKnown {
		compression = ptr(round4(cs))
	}
	return spent, compression
}
