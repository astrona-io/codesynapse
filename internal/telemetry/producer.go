package telemetry

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Produce bridges the bounded registry to the SDK without another accumulator.
// SDK overflow aggregation cannot remove tenant attribution on this path.
func (r *Registry) Produce(ctx context.Context) ([]metricdata.ScopeMetrics, error) {
	r.gaugesMu.RLock()
	defer r.gaugesMu.RUnlock()
	r.mu.Lock()
	ms := slices.Clone(r.metrics)
	r.mu.Unlock()
	scope := metricdata.ScopeMetrics{Scope: instrumentation.Scope{Name: "codesynapse/internal/telemetry"}}
	at := time.Now()
	for _, m := range ms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch m := m.(type) {
		case *Counter:
			scope.Metrics = append(scope.Metrics, m.snapshot(at)...)
		case *Histogram:
			scope.Metrics = append(scope.Metrics, m.snapshot(at)...)
		case *Gauge:
			scope.Metrics = append(scope.Metrics, m.snapshot(at)...)
		}
	}
	return []metricdata.ScopeMetrics{scope}, nil
}

func (f *family) attributes(key string) attribute.Set {
	values := strings.Split(key, "\xff")
	attrs := make([]attribute.KeyValue, 0, len(f.labels))
	for i, label := range f.labels {
		if f.scoped && label == "tenant_id" && !tenantRe.MatchString(values[i]) {
			continue
		}
		attrs = append(attrs, attribute.String(label, values[i]))
	}
	return attribute.NewSet(attrs...)
}

func metricUnit(name string) string {
	switch {
	case strings.Contains(name, "_seconds"):
		return "s"
	case strings.Contains(name, "_bytes_"), strings.HasSuffix(name, "_bytes"):
		return "By"
	case strings.Contains(name, "_dollars"):
		return "{USD}"
	default:
		return "1"
	}
}

func (c *Counter) snapshot(at time.Time) []metricdata.Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	groups := map[string][]metricdata.DataPoint[float64]{}
	for key, v := range c.vals {
		name := c.sampleName(key)
		groups[name] = append(groups[name], metricdata.DataPoint[float64]{Attributes: c.attributes(key), StartTime: time.UnixMilli(c.started[key]), Time: at, Value: v})
	}
	out := make([]metricdata.Metrics, 0, len(groups))
	for name, points := range groups {
		out = append(out, metricdata.Metrics{Name: name, Description: c.help, Unit: metricUnit(name), Data: metricdata.Sum[float64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true, DataPoints: points}})
	}
	return out
}

func (h *Histogram) snapshot(at time.Time) []metricdata.Metrics {
	h.mu.Lock()
	defer h.mu.Unlock()
	groups := map[string][]metricdata.HistogramDataPoint[float64]{}
	for key, v := range h.vals {
		counts := make([]uint64, len(v.counts)+1)
		copy(counts, v.counts)
		counts[len(v.counts)] = v.count
		for _, n := range v.counts {
			counts[len(v.counts)] -= n
		}
		name := h.sampleName(key)
		groups[name] = append(groups[name], metricdata.HistogramDataPoint[float64]{Attributes: h.attributes(key), StartTime: time.UnixMilli(h.started[key]), Time: at, Bounds: slices.Clone(h.buckets), BucketCounts: counts, Sum: v.sum, Count: v.count})
	}
	out := make([]metricdata.Metrics, 0, len(groups))
	for name, points := range groups {
		out = append(out, metricdata.Metrics{Name: name, Description: h.help, Unit: metricUnit(name), Data: metricdata.Histogram[float64]{Temporality: metricdata.CumulativeTemporality, DataPoints: points}})
	}
	return out
}

func (g *Gauge) snapshot(at time.Time) []metricdata.Metrics {
	g.mu.Lock()
	defer g.mu.Unlock()
	groups := map[string][]metricdata.DataPoint[float64]{}
	for key, v := range g.vals {
		name := g.sampleName(key)
		groups[name] = append(groups[name], metricdata.DataPoint[float64]{Attributes: g.attributes(key), Time: at, Value: v})
	}
	out := make([]metricdata.Metrics, 0, len(groups))
	for name, points := range groups {
		out = append(out, metricdata.Metrics{Name: name, Description: g.help, Unit: metricUnit(name), Data: metricdata.Gauge[float64]{DataPoints: points}})
	}
	return out
}
