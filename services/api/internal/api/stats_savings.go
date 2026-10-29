package api

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Savings over time for Statistics → Models: the window of GET /api/v1/stats
// cut into UTC buckets, hourly for one day, six-hourly up to a week, daily
// up to 30 days and weekly beyond. Each bucket carries what the page's money
// tiles are made of, computed by the same functions, so the buckets add up
// to the tiles of the same window:
//
//   - graph answers (tokens never read and their dollars) from the daily
//     usage counters through agentUsage and savedEstimate;
//   - local model work, prompt compression and the gateway's local answers
//     from the metrics store, with fixed MetricsQL and the caller's tenants
//     as extra_filters.
//
// The usage counters are daily. Below a day, each day's graph answers are
// placed over its hours in proportion to the tokens the navigation tools
// returned per hour (mcp_response_tokens_total); a day with no such
// measurement is left unplaced and counted apart.

// Bucket sizes, as the API names them.
const (
	savingsHour  = "hour"
	savingsSix   = "6h"
	savingsDay   = "day"
	savingsWeek  = "week"
	savingsWeeks = 30 // windows longer than this many days are cut into weeks
)

// savingsLayout is a window cut into buckets. bounds holds len(buckets)+1
// UTC instants; slot is the step the metrics store is queried with.
type savingsLayout struct {
	step   string
	slot   time.Duration
	bounds []time.Time
}

// layoutFor cuts the days ending today (UTC) the way handleStats counts them:
// from midnight days-1 days ago to the coming midnight.
func layoutFor(days int, now time.Time) savingsLayout {
	today := now.UTC().Truncate(24 * time.Hour)
	start, end := today.AddDate(0, 0, -(days-1)), today.AddDate(0, 0, 1)
	l := savingsLayout{step: savingsDay, slot: 24 * time.Hour}
	switch {
	case days == 1:
		l.step, l.slot = savingsHour, time.Hour
	case days <= 7:
		l.step, l.slot = savingsSix, 6*time.Hour
	case days > savingsWeeks:
		// Weeks run Monday to Sunday; the first and last may be partial.
		l.step = savingsWeek
		l.bounds = []time.Time{start}
		for t := start.AddDate(0, 0, 1); t.Before(end); t = t.AddDate(0, 0, 1) {
			if t.Weekday() == time.Monday {
				l.bounds = append(l.bounds, t)
			}
		}
		l.bounds = append(l.bounds, end)
		return l
	}
	for t := start; !t.After(end); t = t.Add(l.slot) {
		l.bounds = append(l.bounds, t)
	}
	return l
}

// SavingsBucket is one bucket of the savings chart. Dollars are nil when
// they cannot be valued (unknown, never zero). The local model tokens are
// the inputs of the page's local saving, which it prices like the tiles.
type SavingsBucket struct {
	Start int64 `json:"start"` // unix seconds, UTC
	End   int64 `json:"end"`
	// NeverReadTokens is the graph answers' part of tokens avoided
	// (ask_local is local model work) and GraphSaved its dollars,
	// savedEstimate's TotalDollars.
	NeverReadTokens   int      `json:"never_read_tokens"`
	GraphSaved        *float64 `json:"graph_saved_dollars"`
	LocalInputTokens  float64  `json:"local_input_tokens"`
	LocalOutputTokens float64  `json:"local_output_tokens"`
	LocalCachedTokens float64  `json:"local_cached_tokens"`
	CompressionSaved  *float64 `json:"compression_saved_dollars"`
	GatewaySaved      *float64 `json:"gateway_saved_dollars"`
}

// SavingsSeries is the answer of GET /api/v1/stats/savings.
type SavingsSeries struct {
	Days int `json:"days"`
	// Step is hour, 6h, day or week (Monday to Sunday, UTC).
	Step    string          `json:"step"`
	Buckets []SavingsBucket `json:"buckets"`
	// Metrics is false when the metrics store is not configured or did not
	// answer; local model work is then missing, and sub-day graph answers
	// cannot be placed.
	Metrics bool `json:"metrics"`
	// Unplaced counts graph-answer tokens (and their dollars) of days the
	// hourly measurement does not cover; they are in the window's tiles but
	// in no sub-day bucket.
	UnplacedTokens  int             `json:"unplaced_never_read_tokens"`
	UnplacedDollars float64         `json:"unplaced_graph_saved_dollars"`
	Reference       *PriceReference `json:"reference_price"`
}

// savingsQueries; $w is the slot. The graph shape sums the navigation tools'
// response tokens, the base of their avoided figure (agentStats).
//
//nolint:gosec // metric names, not credentials
var savingsQueries = struct{ input, output, cached, compressed, gateway, unmetered, graphShape string }{
	input:      `sum by (llm_provider)(increase(llm_tokens_input_total[$w]))`,
	output:     `sum by (llm_provider)(increase(llm_tokens_output_total[$w]))`,
	cached:     `sum by (llm_provider)(increase(llm_tokens_saved_total[$w]))`,
	compressed: `sum by (llm_provider)(increase(llm_compressed_bytes_total[$w]))`,
	gateway:    `sum by (kind)(increase(gateway_local_tokens_total[$w]))`,
	unmetered:  `sum(increase(gateway_local_unmetered_total[$w]))`,
	graphShape: `sum(increase(mcp_response_tokens_total{mcp_tool=~"` + navigationToolsRegex() + `"}[$w]))`,
}

// navigationToolsRegex is a constant alternation of the tool names, sorted
// so the query text never changes between runs.
func navigationToolsRegex() string {
	names := make([]string, 0, len(navigationTools))
	for t := range navigationTools {
		names = append(names, t)
	}
	slices.Sort(names)
	return strings.Join(names, "|")
}

// savingsSlot is one metrics-store slot of the window.
type savingsSlot struct {
	model      map[string]*ModelStat // per provider: tokens and compressed bytes
	gw         GatewayStat
	graphShape float64
}

// metricsSlots reads the window's slots from the metrics store. Points are
// stamped at the end of their slot; the last slot may end in the future,
// which counts what has happened so far.
func (s *Server) metricsSlots(ctx context.Context, l savingsLayout, tenants []string) ([]savingsSlot, error) {
	start, end := l.bounds[0], l.bounds[len(l.bounds)-1]
	n := int(end.Sub(start) / l.slot)
	slots := make([]savingsSlot, n)
	for i := range slots {
		slots[i].model = map[string]*ModelStat{}
	}
	window := strconv.Itoa(int(l.slot/time.Second)) + "s"
	model := func(i int, p string) *ModelStat {
		m := slots[i].model[p]
		if m == nil {
			m = &ModelStat{Provider: p}
			if s.cfg.LLM != nil {
				if cfg, ok := s.cfg.LLM.Config(p); ok {
					m.Configured, m.Local = true, cfg.Local()
				}
			}
			slots[i].model[p] = m
		}
		return m
	}
	steps := []struct {
		expr string
		add  func(i int, l map[string]string, v float64)
	}{
		{savingsQueries.input, func(i int, l map[string]string, v float64) { model(i, l["llm_provider"]).InputTokens += v }},
		{savingsQueries.output, func(i int, l map[string]string, v float64) { model(i, l["llm_provider"]).OutputTokens += v }},
		{savingsQueries.cached, func(i int, l map[string]string, v float64) { model(i, l["llm_provider"]).CachedTokens += v }},
		{savingsQueries.compressed, func(i int, l map[string]string, v float64) { model(i, l["llm_provider"]).CompressedBytes += v }},
		{savingsQueries.gateway, func(i int, l map[string]string, v float64) {
			switch l["kind"] {
			case "input":
				slots[i].gw.LocalInputTokens += v
			case "output":
				slots[i].gw.LocalOutputTokens += v
			case "cached":
				slots[i].gw.LocalCachedTokens += v
			}
		}},
		{savingsQueries.unmetered, func(i int, _ map[string]string, v float64) { slots[i].gw.LocalUnmetered += v }},
		{savingsQueries.graphShape, func(i int, _ map[string]string, v float64) { slots[i].graphShape += v }},
	}
	for _, st := range steps {
		res, err := s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(st.expr, "$w", window), start.Add(l.slot), end, l.slot, tenants)
		if err != nil {
			return nil, err
		}
		for _, ts := range res {
			for _, p := range ts.Points {
				i := int(time.Unix(p.T, 0).Sub(start)/l.slot) - 1
				if i < 0 || i >= n || math.IsNaN(p.V) || math.IsInf(p.V, 0) {
					continue
				}
				st.add(i, ts.Labels, p.V)
			}
		}
	}
	return slots, nil
}

// buildSavings fills the buckets of l from the usage counters and, when
// slots is not nil, the metrics store's slots.
func (s *Server) buildSavings(l savingsLayout, buckets []usage.Bucket, repos []RepoSummary, slots []savingsSlot) SavingsSeries {
	ref := s.priceReference()
	out := SavingsSeries{Step: l.step, Buckets: make([]SavingsBucket, len(l.bounds)-1), Metrics: slots != nil, Reference: &ref}
	start := l.bounds[0]
	out.Days = int(l.bounds[len(l.bounds)-1].Sub(start) / (24 * time.Hour))
	for i := range out.Buckets {
		out.Buckets[i] = SavingsBucket{Start: l.bounds[i].Unix(), End: l.bounds[i+1].Unix()}
	}
	// graph is one span of whole days valued exactly as the tiles value a
	// window: agentUsage, then savedEstimate.
	graph := func(from, to time.Time) (int, *float64) {
		days := int(to.Sub(from) / (24 * time.Hour))
		a := agentUsage(buckets, repos, days, to.Add(-time.Hour))
		a.Saved = savedEstimate(a, ref)
		return a.AvoidedTokens - a.LocalAvoidedTokens, a.Saved.TotalDollars
	}
	if l.slot >= 24*time.Hour {
		for i := range out.Buckets {
			out.Buckets[i].NeverReadTokens, out.Buckets[i].GraphSaved = graph(l.bounds[i], l.bounds[i+1])
		}
	} else {
		perDay := int(24 * time.Hour / l.slot)
		for d := 0; d*perDay < len(out.Buckets); d++ {
			from := start.Add(time.Duration(d) * 24 * time.Hour)
			tokens, dollars := graph(from, from.Add(24*time.Hour))
			var weights []float64
			if slots != nil {
				for j := range perDay {
					weights = append(weights, slots[d*perDay+j].graphShape)
				}
			}
			parts := apportion(tokens, weights)
			if parts == nil {
				out.UnplacedTokens += tokens
				if dollars != nil {
					out.UnplacedDollars = round4(out.UnplacedDollars + *dollars)
				}
				continue
			}
			for j, n := range parts {
				b := &out.Buckets[d*perDay+j]
				b.NeverReadTokens = n
				if dollars != nil {
					b.GraphSaved = ptr(round4(*dollars * weights[j] / sum(weights)))
				}
			}
		}
	}
	if slots == nil {
		return out
	}
	// A bucket holds one slot, or the daily slots of its week. The money
	// of each slot is valued like the tiles value the window; a slot that
	// cannot be valued makes its bucket unknown.
	started := make([]bool, len(out.Buckets))
	for k := range slots {
		i := bucketOf(l.bounds, start.Add(time.Duration(k)*l.slot))
		b := &out.Buckets[i]
		models := make([]ModelStat, 0, len(slots[k].model))
		for _, m := range slots[k].model {
			if m.Local {
				b.LocalInputTokens += m.InputTokens
				b.LocalOutputTokens += m.OutputTokens
				b.LocalCachedTokens += m.CachedTokens
			}
			models = append(models, *m)
		}
		_, compression := s.moneyTotals(models)
		gw := slots[k].gw
		var saved *float64
		if gw.LocalUnmetered == 0 {
			saved = ptr(gatewaySaved(gw, ref))
		}
		if !started[i] {
			started[i] = true
			b.CompressionSaved, b.GatewaySaved = compression, saved
			continue
		}
		b.CompressionSaved, b.GatewaySaved = addKnown(b.CompressionSaved, compression), addKnown(b.GatewaySaved, saved)
	}
	for i := range out.Buckets {
		b := &out.Buckets[i]
		b.LocalInputTokens, b.LocalOutputTokens, b.LocalCachedTokens = math.Round(b.LocalInputTokens), math.Round(b.LocalOutputTokens), math.Round(b.LocalCachedTokens)
	}
	return out
}

// bucketOf is the bucket whose span holds t.
func bucketOf(bounds []time.Time, t time.Time) int {
	i, found := slices.BinarySearchFunc(bounds, t, func(b, t time.Time) int { return b.Compare(t) })
	if !found {
		i--
	}
	return max(0, min(i, len(bounds)-2))
}

// addKnown adds two dollar figures; an unknown part makes the sum unknown.
func addKnown(acc, v *float64) *float64 {
	if acc == nil || v == nil {
		return nil
	}
	return ptr(round4(*acc + *v))
}

// apportion splits total over weights with the largest remainder, so the
// parts add up to total exactly. It is nil when the weights are all zero.
func apportion(total int, weights []float64) []int {
	w := sum(weights)
	if w <= 0 {
		if total == 0 && len(weights) > 0 {
			return make([]int, len(weights))
		}
		return nil
	}
	parts := make([]int, len(weights))
	rest := make([]int, len(weights))
	left := total
	for i, x := range weights {
		exact := float64(total) * x / w
		parts[i] = int(exact)
		left -= parts[i]
		rest[i] = i
	}
	frac := func(i int) float64 { return float64(total)*weights[i]/w - float64(parts[i]) }
	slices.SortStableFunc(rest, func(a, b int) int {
		switch fa, fb := frac(a), frac(b); {
		case fa > fb:
			return -1
		case fa < fb:
			return 1
		}
		return 0
	})
	for _, i := range rest[:left] {
		parts[i]++
	}
	return parts
}

func sum(xs []float64) float64 {
	var t float64
	for _, x := range xs {
		t += x
	}
	return t
}

func (s *Server) handleSavingsSeries(w http.ResponseWriter, r *http.Request) {
	// The window of GET /api/v1/stats: bad values fall back to 30 days.
	days, _ := strconv.Atoi(r.URL.Query().Get("days")) //nolint:errcheck // see above
	if days <= 0 || days > 180 {
		days = 30
	}
	ctx := r.Context()
	now := time.Now()
	ov, err := s.buildOverview(ctx)
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	buckets, err := s.usageBuckets(ctx, days, now)
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	l := layoutFor(days, now)
	var slots []savingsSlot
	if s.cfg.Metrics != nil {
		// Without the metrics store the graph answers still show; the page
		// says local model work is missing.
		if slots, err = s.metricsSlots(ctx, l, callerTenants(r)); err != nil {
			logx.FromContext(ctx).Warn("savings series: metrics store", "err", err)
			slots = nil
		}
	}
	writeJSON(w, http.StatusOK, s.buildSavings(l, buckets, ov.Repos, slots))
}
