package telemetry

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func counterValue(c *Counter, values ...string) float64 {
	key, _ := c.key(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vals[key]
}

func TestPlatformHealthLabelsStayInTheirSets(t *testing.T) {
	tests := []struct {
		name         string
		lease, event string
		counted      bool
	}{
		{"loops acquired", LeaseLoops, LeaseAcquired, true},
		{"loops lost", LeaseLoops, LeaseLost, true},
		{"job renew failed", LeaseJob, LeaseRenewFailed, true},
		{"unknown lease", "repo/acme", LeaseLost, false},
		{"unknown event", LeaseJob, "stolen", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := counterValue(LeaseEvents, tt.lease, tt.event)
			RecordLease(context.Background(), tt.lease, tt.event)
			assert.Equal(t, counterValue(LeaseEvents, tt.lease, tt.event)-before, map[bool]float64{true: 1, false: 0}[tt.counted])
		})
	}
	t.Run("exporters", func(t *testing.T) {
		before := counterValue(exportFailures, "secret.example")
		ExportFailed(context.Background(), "secret.example")
		assert.Equal(t, counterValue(exportFailures, "secret.example"), before, "unknown exporters are dropped")
		before = counterValue(exportFailures, ExporterIngest)
		ExportFailed(context.Background(), ExporterIngest)
		assert.Equal(t, counterValue(exportFailures, ExporterIngest), before+1)
	})
}

// TestPlatformHealthHasNoTenant: process instruments are platform_ series
// with no tenant, tool or model attribute, even when recorded with a tenant
// in the context.
func TestPlatformHealthHasNoTenant(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("platform_test_goroutines", "Goroutines.")
	pool := r.Gauge("platform_test_pool", "Pool.", "db_system", "state")
	c := r.Counter("platform_test_cpu_seconds_total", "CPU.")
	ctx := WithTool(WithTenant(context.Background(), "111111111111111111111111"), "search")
	g.SetContext(ctx, 12)
	pool.SetContext(ctx, 3, DBSystemPostgres, "idle")
	c.AddContext(ctx, 1.5)
	var text bytes.Buffer
	require.NoError(t, r.WriteText(&text))
	assert.Contains(t, text.String(), "platform_test_goroutines 12\n")
	assert.Contains(t, text.String(), `platform_test_pool{db_system="postgres",state="idle"} 3`)
	assert.Contains(t, text.String(), "platform_test_cpu_seconds_total 1.5\n")
	for _, label := range coreLabels {
		assert.False(t, strings.Contains(text.String(), label), label)
	}
	scopes, err := r.Produce(ctx)
	require.NoError(t, err)
	for _, m := range scopes[0].Metrics {
		assert.True(t, strings.HasPrefix(m.Name, "platform_") || m.Name == "telemetry_dropped_series_total", m.Name)
		if gauge, ok := m.Data.(metricdata.Gauge[float64]); ok {
			for _, p := range gauge.DataPoints {
				_, has := p.Attributes.Value("tenant_id")
				assert.False(t, has, "gauges carry no tenant")
			}
		}
	}
	assert.Equal(t, metricUnit("platform_go_heap_bytes"), "By")
}

func TestUnscopedGaugeMustBePlatform(t *testing.T) {
	defer func() { assert.True(t, recover() != nil, "a tenant-looking unscoped gauge is a programmer error") }()
	NewRegistry().Gauge("goroutines", "Goroutines.")
}

func TestTrackedPoolsAreSummedAndReleased(t *testing.T) {
	a := TrackPool(DBSystemPostgres, func() PoolStat {
		return PoolStat{Idle: 2, InUse: 1, Max: 10, EmptyAcquires: 4, EmptyAcquireWait: time.Second}
	})
	b := TrackPool(DBSystemPostgres, func() PoolStat { return PoolStat{Idle: 1, Max: 5} })
	TrackPool("mysql", func() PoolStat { return PoolStat{Idle: 99} })()
	got := PoolStats()[DBSystemPostgres]
	assert.Equal(t, got, PoolStat{Idle: 3, InUse: 1, Max: 15, EmptyAcquires: 4, EmptyAcquireWait: time.Second})
	_, other := PoolStats()["mysql"]
	assert.False(t, other, "only fixed db_system values are tracked")
	a()
	a() // idempotent
	b()
	_, left := PoolStats()[DBSystemPostgres]
	assert.False(t, left)
}

func TestFailedOTLPExportIsCounted(t *testing.T) {
	r := NewRegistry()
	r.Counter("tokens_total", "Tokens.").Add(1)
	scopes, err := r.Produce(context.Background())
	require.NoError(t, err)
	inner := &testMetricExporter{fail: true}
	e := &firstValueExporter{Exporter: inner, seen: map[seriesIdentity]bool{}}
	before := counterValue(exportFailures, ExporterOTLP)
	require.Error(t, e.Export(context.Background(), &metricdata.ResourceMetrics{ScopeMetrics: scopes}))
	assert.Equal(t, counterValue(exportFailures, ExporterOTLP), before+1)
}
