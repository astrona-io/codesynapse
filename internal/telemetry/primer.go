package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var errOTLPExport = errors.New("OTLP metric export failed")

type seriesIdentity struct {
	metric     string
	attributes attribute.Distinct
	start      int64
}

// firstValueExporter sends an explicit zero before a cumulative stream's first
// value. VictoriaMetrics otherwise treats a large first value as pre-existing.
// Only admitted counter/histogram series enter seen; their registry sets are bounded.
type firstValueExporter struct {
	sdkmetric.Exporter
	seen map[seriesIdentity]bool
}

func (e *firstValueExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	zero := metricdata.ResourceMetrics{Resource: data.Resource}
	var added []seriesIdentity
	for _, scope := range data.ScopeMetrics {
		out := metricdata.ScopeMetrics{Scope: scope.Scope}
		for _, m := range scope.Metrics {
			z := metricdata.Metrics{Name: m.Name, Description: m.Description, Unit: m.Unit}
			switch v := m.Data.(type) {
			case metricdata.Sum[float64]:
				points := make([]metricdata.DataPoint[float64], 0)
				for _, p := range v.DataPoints {
					id := seriesIdentity{m.Name, p.Attributes.Equivalent(), p.StartTime.UnixNano()}
					if e.seen[id] {
						continue
					}
					added = append(added, id)
					p.Value = 0
					p.Time = p.StartTime.Add(-time.Millisecond)
					p.StartTime = p.Time
					points = append(points, p)
				}
				if len(points) > 0 {
					z.Data = metricdata.Sum[float64]{DataPoints: points, IsMonotonic: true, Temporality: metricdata.CumulativeTemporality}
				}
			case metricdata.Histogram[float64]:
				points := make([]metricdata.HistogramDataPoint[float64], 0)
				for _, p := range v.DataPoints {
					id := seriesIdentity{m.Name, p.Attributes.Equivalent(), p.StartTime.UnixNano()}
					if e.seen[id] {
						continue
					}
					added = append(added, id)
					p.Count, p.Sum = 0, 0
					p.BucketCounts = make([]uint64, len(p.BucketCounts))
					p.Time = p.StartTime.Add(-time.Millisecond)
					p.StartTime = p.Time
					points = append(points, p)
				}
				if len(points) > 0 {
					z.Data = metricdata.Histogram[float64]{DataPoints: points, Temporality: metricdata.CumulativeTemporality}
				}
			}
			if z.Data != nil {
				out.Metrics = append(out.Metrics, z)
			}
		}
		if len(out.Metrics) > 0 {
			zero.ScopeMetrics = append(zero.ScopeMetrics, out)
		}
	}
	if len(added) > 0 {
		if err := e.Exporter.Export(ctx, &zero); err != nil {
			return exportError(ctx)
		}
		for _, id := range added {
			e.seen[id] = true
		}
	}
	if err := e.Exporter.Export(ctx, data); err != nil {
		return exportError(ctx)
	}
	return nil
}

// Exporter errors may contain receiver response bodies; keep them out of logs.
func exportError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errOTLPExport
}
