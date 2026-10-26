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
}

// ModelStats is the answer of GET /api/v1/stats/models.
type ModelStats struct {
	Range  string      `json:"range"`
	Models []ModelStat `json:"models"`
	// Reference prices a cloud model the operator configured, so work done
	// locally can be valued at what that model would have charged; nil when
	// no cloud provider has prices, and then no cost is estimated.
	Reference *PriceReference `json:"reference_price,omitempty"`
}

// PriceReference is one configured cloud provider's prices, US dollars per
// million tokens.
type PriceReference struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// Source and Checked say where the prices come from and when they
	// were read (price_source and price_checked, or the price feed).
	Source  string `json:"source,omitempty"`
	Checked string `json:"checked,omitempty"`
}

// modelQueries; $r is the window.
var modelQueries = struct{ requests, input, output, readS, writeS, readT, writeT, p50, p95 string }{
	requests: `sum by (llm_provider, llm_model, task, outcome)(increase(llm_requests_total[$r]))`,
	input:    `sum by (llm_provider, llm_model, task)(increase(llm_tokens_input_total[$r]))`,
	output:   `sum by (llm_provider, llm_model, task)(increase(llm_tokens_output_total[$r]))`,
	readS:    `sum by (llm_provider, llm_model)(increase(llm_read_seconds_total[$r]))`,
	writeS:   `sum by (llm_provider, llm_model)(increase(llm_write_seconds_total[$r]))`,
	readT:    `sum by (llm_provider, llm_model)(increase(llm_read_tokens_total[$r]))`,
	writeT:   `sum by (llm_provider, llm_model)(increase(llm_write_tokens_total[$r]))`,
	p50:      `histogram_quantile(0.5, sum by (le, llm_provider, llm_model)(increase(llm_context_used_ratio_bucket[$r])))`,
	p95:      `histogram_quantile(0.95, sum by (le, llm_provider, llm_model)(increase(llm_context_used_ratio_bucket[$r])))`,
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
			if l["outcome"] == "error" {
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
		{modelQueries.readS, func(l map[string]string, v float64) { speedOf(l)[0] += v }},
		{modelQueries.writeS, func(l map[string]string, v float64) { speedOf(l)[1] += v }},
		{modelQueries.readT, func(l map[string]string, v float64) { speedOf(l)[2] += v }},
		{modelQueries.writeT, func(l map[string]string, v float64) { speedOf(l)[3] += v }},
		{modelQueries.p50, func(l map[string]string, v float64) { stat(l).ContextP50 = ptr(round3(v)) }},
		{modelQueries.p95, func(l map[string]string, v float64) { stat(l).ContextP95 = ptr(round3(v)) }},
	}
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
				out.Reference = &PriceReference{Provider: p.Name, Model: p.Model, InputPerMTok: p.PriceInputPerMTok, OutputPerMTok: p.PriceOutputPerMTok, Source: p.PriceSource, Checked: p.PriceChecked}
				break
			}
		}
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
	slices.SortFunc(out.Models, func(a, b ModelStat) int {
		return cmp.Or(cmp.Compare(b.InputTokens+b.OutputTokens, a.InputTokens+a.OutputTokens), cmp.Compare(a.Provider+a.Model, b.Provider+b.Model))
	})
	writeJSON(w, http.StatusOK, out)
}

func ptr(v float64) *float64 { return &v }

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
