package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestScopedMetricsUseTrustedContext(t *testing.T) {
	r := NewRegistry()
	AllowTools("accepted_test_tool")
	AllowModels("configured", "configured-model")
	ctx := WithModel(WithTool(WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa"), "accepted_test_tool"), "configured", "configured-model")
	c := r.ScopedCounter("observations_total", "Observations.", "tenant_id", "mcp_tool", "llm_provider", "llm_model")
	c.AddContext(ctx, 3, "bbbbbbbbbbbbbbbbbbbbbbbb", "forged-tool", "forged-provider", "forged-model")
	out := text(t, r)
	assert.Contains(t, out, `tenant_id="aaaaaaaaaaaaaaaaaaaaaaaa",mcp_tool="accepted_test_tool",llm_provider="configured",llm_model="configured-model"} 3`)
	assert.False(t, strings.Contains(out, "forged"))
	assert.False(t, strings.Contains(out, "bbbbbbbbbbbbbbbbbbbbbbbb"))
	c.IncContext(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa", "forged-tool", "configured", "configured-model")
	out = text(t, r)
	assert.Contains(t, out, `platform_observations_total{mcp_tool="none",llm_provider="none",llm_model="none"} 1`)
}

func TestHistogramBudgetIncludesExportedBuckets(t *testing.T) {
	r := NewRegistry()
	h := r.ScopedHistogram("roundtrip_seconds", "Latency.", latency, "tenant_id")
	for i := range maxSeries {
		h.ObserveContext(WithTenant(context.Background(), fmt.Sprintf("%024x", i)), 0.01, "")
	}
	assert.True(t, len(h.vals)*(len(h.buckets)+3) <= maxSeries)
	assert.Equal(t, r.dropped.vals["roundtrip_seconds"], float64(maxSeries-h.budget))
	assert.False(t, strings.Contains(text(t, r), "otel.metric.overflow"))
}

func TestGaugeDeletionAndUntrustedIdentity(t *testing.T) {
	r := NewRegistry()
	g := r.ScopedGauge("active_facts", "Facts.", "tenant_id")
	ctx := WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa")
	g.SetContext(ctx, 4, "")
	g.SetContext(ctx, 2, "")
	assert.Contains(t, text(t, r), `} 2`)
	g.DeleteContext(ctx, "")
	assert.Equal(t, len(g.vals), 0)
	ctx = WithModel(WithTool(WithTenant(ctx, "client-controlled"), "unregistered"), "unregistered", "unregistered")
	g.SetContext(ctx, 1, "")
	out := text(t, r)
	assert.Contains(t, out, `platform_active_facts{mcp_tool="unknown",llm_provider="none",llm_model="none"} 1`)
	assert.False(t, strings.Contains(out, "tenant_id="))
}

func TestJobsWaitingIsAPlatformSeries(t *testing.T) {
	r := NewRegistry()
	g := r.ScopedGauge("codesynapse_jobs_waiting", "Jobs.", "kind")
	g.SetContext(context.Background(), 3, "sync")
	out := text(t, r)
	assert.Contains(t, out, `platform_codesynapse_jobs_waiting{`)
	assert.Contains(t, out, `kind="sync",mcp_tool="none",llm_provider="none",llm_model="none"} 3`)
	assert.False(t, strings.Contains(out, "tenant_id="))
}

func BenchmarkRecord(b *testing.B) {
	ctx := WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa")
	for _, scoped := range []bool{false, true} {
		b.Run(fmt.Sprintf("attribution=%t", scoped), func(b *testing.B) {
			r := NewRegistry()
			c := r.Counter("events_total", "Events.", "tenant_id")
			if scoped {
				c.scope()
			}
			b.ReportAllocs()
			for b.Loop() {
				c.AddContext(ctx, 1, Tenant(ctx))
			}
		})
	}
}
