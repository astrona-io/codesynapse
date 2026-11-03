package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/services/api/internal/gateway"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// Feature health (docs/plans/FEATURE_HEALTH_PLAN.md, phase 1): how long the
// optional features and the tools take, per feature and client, from the
// hourly usage buckets. Only counts and durations are kept; never prompts,
// paths, users or sessions.

// timingFlush is how often the API's own timings (the gateway's local
// answers and prompt shortening) are added to the store.
const timingFlush = 30 * time.Second

// gatewayClient is the client the gateway's timings are counted under: the
// gateway serves agents that do not name themselves to it.
const gatewayClient = "gateway"

// featureWindows are the windows GET /api/v1/stats/features answers, in
// hours back from the start of the current hour (which is included).
var featureWindows = map[string]int{"1h": 1, "24h": 23, "7d": 7*24 - 1}

// FeatureTiming is one feature's (or one client's share of it) calls in a
// window. Percentiles are estimated from the latency histogram.
type FeatureTiming struct {
	Calls    int `json:"calls"`
	Errors   int `json:"errors"`
	Timeouts int `json:"timeouts"`
	MinMs    int `json:"min_ms"`
	MaxMs    int `json:"max_ms"`
	AvgMs    int `json:"avg_ms"`
	P50Ms    int `json:"p50_ms"`
	P95Ms    int `json:"p95_ms"`
}

// ClientTiming is a feature's timing for one client.
type ClientTiming struct {
	Client string `json:"client"`
	FeatureTiming
}

// FeatureHealth is one feature in the window.
type FeatureHealth struct {
	// Feature is the tool name, or the short name of a pseudo feature
	// ("pre-brief").
	Feature string `json:"feature"`
	// Kind is model (a self-hosted model answers it), hook (the plugin's
	// hooks) or tool (an MCP tool).
	Kind    string         `json:"kind"`
	Total   FeatureTiming  `json:"total"`
	Clients []ClientTiming `json:"clients"`
}

// FeatureHealthStats is GET /api/v1/stats/features.
type FeatureHealthStats struct {
	Window      string          `json:"window"`
	Since       string          `json:"since"`
	GeneratedAt string          `json:"generated_at"`
	Bounds      []int           `json:"latency_bounds_ms"`
	Features    []FeatureHealth `json:"features"`
}

func (s *Server) handleFeatureHealth(w http.ResponseWriter, r *http.Request) {
	window := cmp.Or(r.URL.Query().Get("window"), "24h")
	hours, ok := featureWindows[window]
	if !ok {
		s.problem(w, http.StatusBadRequest, errors.New("window must be 1h, 24h or 7d"))
		return
	}
	now := time.Now().UTC()
	since := now.Truncate(time.Hour).Add(-time.Duration(hours) * time.Hour)
	buckets, err := s.st.UsageHours(r.Context(), usage.Day(since))
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	buckets = visibleUsage(scopeFrom(r.Context()), buckets)
	writeJSON(w, http.StatusOK, FeatureHealthStats{Window: window, Since: since.Format(time.RFC3339), GeneratedAt: now.Format(time.RFC3339),
		Bounds: usage.LatencyBounds[:], Features: featureHealth(buckets, since)})
}

// featureHealth merges the timed hourly buckets from since on per feature and
// client. Features come slowest first by p95, then by name.
func featureHealth(buckets []usage.Bucket, since time.Time) []FeatureHealth {
	type key struct{ tool, client string }
	merged := map[key]*usage.Bucket{}
	totals := map[string]*usage.Bucket{}
	for _, b := range buckets {
		start, err := b.Start()
		if err != nil || start.Before(since) || b.Hour == "" || !usage.Timed(b.Tool) || b.Latency.Count() == 0 {
			continue
		}
		addTo(merged, key{b.Tool, b.Client}, b)
		addTo(totals, b.Tool, b)
	}
	out := make([]FeatureHealth, 0, len(totals))
	for tool, t := range totals {
		f := FeatureHealth{Feature: usage.FeatureName(tool), Kind: featureKind(tool), Total: timingOf(*t), Clients: []ClientTiming{}}
		for k, b := range merged {
			if k.tool == tool {
				f.Clients = append(f.Clients, ClientTiming{Client: k.client, FeatureTiming: timingOf(*b)})
			}
		}
		slices.SortFunc(f.Clients, func(a, b ClientTiming) int {
			return cmp.Or(cmp.Compare(b.Calls, a.Calls), cmp.Compare(a.Client, b.Client))
		})
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b FeatureHealth) int {
		return cmp.Or(cmp.Compare(b.Total.P95Ms, a.Total.P95Ms), cmp.Compare(a.Feature, b.Feature))
	})
	return out
}

func addTo[K comparable](m map[K]*usage.Bucket, k K, b usage.Bucket) {
	if cur, ok := m[k]; ok {
		cur.Add(b)
		return
	}
	m[k] = &b
}

// timingOf reads a merged bucket's timing. Calls counts only timed calls:
// hourly buckets hold nothing else.
func timingOf(b usage.Bucket) FeatureTiming {
	n := b.Latency.Count()
	t := FeatureTiming{Calls: n, Errors: b.Errors, Timeouts: b.Timeouts, MinMs: b.MinDurationMs, MaxMs: b.MaxDurationMs,
		P50Ms: b.Latency.Quantile(0.5, b.MinDurationMs, b.MaxDurationMs), P95Ms: b.Latency.Quantile(0.95, b.MinDurationMs, b.MaxDurationMs)}
	if n > 0 {
		t.AvgMs = b.DurationMs / n
	}
	return t
}

// featureKind sorts a timed tool into model, hook or tool.
func featureKind(tool string) string {
	switch {
	case slices.Contains(usage.ModelFeatures, tool):
		return "model"
	case slices.Contains(usage.HookFeatures, tool):
		return "hook"
	}
	return "tool"
}

// timeGateway counts the gateway's own model calls of one request: the local
// answer (until its first byte when streamed) and the prompt shortening.
// A request the agent cancelled says nothing about the model and is not
// counted.
func (s *Server) timeGateway(m gateway.Meta, org string) {
	if s.timings == nil {
		return
	}
	if d, o, ok := gatewayLocalTiming(m); ok {
		s.timings.RecordOutcome(org, "", usage.FeatureGatewayLocal, gatewayClient, d, 0, o)
	}
	if m.ShortenAsked {
		o := usage.OK
		switch {
		case m.ShortenSkip == gateway.SkipTimeout:
			o = usage.TimedOut
		case m.ShortenFailed:
			o = usage.Failed
		}
		s.timings.RecordOutcome(org, "", usage.FeatureShorten, gatewayClient, m.ShortenTook, 0, o)
	}
}

func gatewayLocalTiming(m gateway.Meta) (time.Duration, usage.Outcome, bool) {
	if !m.Asked || m.Fallthrough == gateway.FallCancelled {
		return 0, usage.OK, false
	}
	o := usage.OK
	switch m.Fallthrough {
	case gateway.FallTimeout, gateway.FallQueueTimeout:
		o = usage.TimedOut
	case gateway.FallModelError, gateway.FallBadStatus, gateway.FallEmptyAnswer, gateway.FallTruncated, gateway.FallBadAnswer:
		o = usage.Failed
	}
	d := m.QueueWait + m.LocalAnswer
	if m.Route == "local" && m.Stream && m.FirstByte > 0 {
		d = m.FirstByte
	}
	return d, o, true
}

// runTimings flushes the API's own timings until ctx ends.
func (s *Server) runTimings(ctx context.Context) {
	s.timings.Run(ctx, timingFlush, func(err error) { s.logger.Warn("feature timings not saved", "error", err) })
}
