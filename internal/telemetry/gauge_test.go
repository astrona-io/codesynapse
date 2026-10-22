package telemetry

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestGaugeBatchCannotExportHalfAnObservation(t *testing.T) {
	for _, format := range []string{"otlp", "prometheus"} {
		t.Run(format, func(t *testing.T) {
			r := NewRegistry()
			const tenant = "111111111111111111111111"
			ctx := WithTenant(context.Background(), tenant)
			count := r.ScopedGauge("paired_count", "Count.", "tenant_id")
			observed := r.ScopedGauge("paired_observed_at", "Observation time.", "tenant_id")
			count.SetContext(ctx, 1, tenant)
			observed.SetContext(ctx, 1, tenant)
			halfway, finishUpdate, updated := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(updated)
				r.UpdateGauges(func() {
					count.SetContext(ctx, 2, tenant)
					close(halfway)
					<-finishUpdate
					observed.SetContext(ctx, 2, tenant)
				})
			}()
			<-halfway
			type result struct {
				scopes []metricdata.ScopeMetrics
				text   string
				err    error
			}
			started, exported := make(chan struct{}), make(chan result, 1)
			go func() {
				close(started)
				out := result{}
				if format == "otlp" {
					out.scopes, out.err = r.Produce(ctx)
				} else {
					var body bytes.Buffer
					out.err = r.WriteText(&body)
					out.text = body.String()
				}
				exported <- out
			}()
			<-started
			select {
			case <-exported:
				close(finishUpdate)
				<-updated
				t.Fatal("export escaped while the related gauges were only partly updated")
			case <-time.After(50 * time.Millisecond):
			}
			close(finishUpdate)
			<-updated
			select {
			case out := <-exported:
				require.NoError(t, out.err)
				if format == "otlp" {
					require.Len(t, out.scopes, 1)
					require.Len(t, out.scopes[0].Metrics, 2)
					for _, m := range out.scopes[0].Metrics {
						g, ok := m.Data.(metricdata.Gauge[float64])
						require.True(t, ok)
						require.Len(t, g.DataPoints, 1)
						assert.Equal(t, g.DataPoints[0].Value, 2.0)
					}
				} else {
					lines := 0
					for _, line := range strings.Split(out.text, "\n") {
						if strings.HasPrefix(line, "paired_") {
							assert.True(t, strings.HasSuffix(line, " 2"), line)
							lines++
						}
					}
					assert.Equal(t, lines, 2)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("export remained blocked after the gauge batch completed")
			}
		})
	}
}
