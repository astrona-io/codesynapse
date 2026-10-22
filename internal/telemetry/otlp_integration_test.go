package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// TestVictoriaMetricsOTLP verifies the native receiver, including names and
// counter reset semantics that an in-process exporter mock cannot establish.
func TestVictoriaMetricsOTLP(t *testing.T) {
	endpoint := os.Getenv("CODESYNAPSE_TEST_METRICS_URL")
	if endpoint == "" {
		t.Skip("CODESYNAPSE_TEST_METRICS_URL not set")
	}
	user, pass := os.Getenv("CODESYNAPSE_TEST_METRICS_USER"), os.Getenv("CODESYNAPSE_TEST_METRICS_PASSWORD")
	q := &Querier{URL: endpoint, User: user, Password: pass}
	prefix := fmt.Sprintf("cs_otlp_%d", time.Now().UnixNano())
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"
	AllowTools("otlp_test_tool")
	AllowModels("otlp_test_provider", "otlp_test_model")
	ctx := t.Context()
	ctx = WithModel(WithTool(WithTenant(ctx, a), "otlp_test_tool"), "otlp_test_provider", "otlp_test_model")
	other := WithTenant(ctx, b)
	config := func(reg *Registry, instance string) OTLPConfig {
		return OTLPConfig{URL: endpoint, User: user, Password: pass, Service: "otlp-test", Instance: instance,
			Version: "test", Environment: "integration", Registry: reg, Interval: time.Hour, Timeout: 5 * time.Second}
	}

	t.Run("native names attributes and tenant isolation", func(t *testing.T) {
		r := NewRegistry()
		calls := r.ScopedCounter(prefix+"_calls_total", "Calls.", "tenant_id")
		burst := r.ScopedCounter(prefix+"_burst_total", "Burst.", "tenant_id")
		hist := r.ScopedHistogram(prefix+"_duration_seconds", "Duration.", []float64{0.1, 0.5, 1}, "tenant_id", "db_system")
		gauge := r.ScopedGauge(prefix+"_facts_count", "Current facts.", "tenant_id")
		calls.AddContext(ctx, 5, b) // Legacy arguments cannot replace trusted attribution.
		calls.AddContext(other, 7, a)
		calls.IncContext(WithTenant(ctx, ""), a)
		burst.AddContext(ctx, 37809, a)
		hist.ObserveContext(ctx, 0.25, a, "neo4j")
		hist.ObserveContext(ctx, 1.25, a, "neo4j")
		gauge.SetContext(WithTenant(ctx, a), 7, a)
		p, err := NewOTLP(ctx, config(r, "native"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, p.Shutdown(context.WithoutCancel(ctx))) })
		require.NoError(t, p.ForceFlush(ctx))

		got := awaitOTLPValue(ctx, t, q, prefix+"_calls_total", []string{a}, 5)
		require.Len(t, got, 1)
		assert.Equal(t, got[0].Labels["tenant_id"], a)
		assert.Equal(t, got[0].Labels["mcp_tool"], "otlp_test_tool")
		assert.Equal(t, got[0].Labels["llm_provider"], "otlp_test_provider")
		assert.Equal(t, got[0].Labels["llm_model"], "otlp_test_model")
		assert.Equal(t, got[0].Labels["job"], "otlp-test")
		assert.Equal(t, got[0].Labels["instance"], "native")
		// The native receiver preserves dots in resource attribute names.
		assert.Equal(t, got[0].Labels["service.name"], "otlp-test")
		assert.Equal(t, got[0].Labels["service.version"], "test")
		assert.Equal(t, got[0].Labels["deployment.environment.name"], "integration")
		awaitOTLPValue(ctx, t, q, "sum("+prefix+"_calls_total)", []string{b}, 7)
		awaitOTLPValue(ctx, t, q, "sum(increase("+prefix+"_burst_total[10m]))", []string{a}, 37809)
		awaitOTLPValue(ctx, t, q, "sum("+prefix+"_duration_seconds_count)", []string{a}, 2)
		awaitOTLPValue(ctx, t, q, "sum("+prefix+"_duration_seconds_sum)", []string{a}, 1.5)
		awaitOTLPValue(ctx, t, q, "sum("+prefix+`_duration_seconds_bucket{le="0.5"})`, []string{a}, 1)
		awaitOTLPValue(ctx, t, q, "sum("+prefix+`_duration_seconds_bucket{le="+Inf"})`, []string{a}, 2)
		awaitOTLPValue(ctx, t, q, prefix+"_facts_count", []string{a}, 7)
		awaitOTLPValue(ctx, t, q, "sum(platform_"+prefix+"_calls_total)", nil, 1)

		end := time.Now().Add(time.Minute)
		none, err := q.QueryRange(ctx, prefix+"_calls_total", end.Add(-5*time.Minute), end, time.Second, []string{})
		require.NoError(t, err)
		assert.Len(t, none, 0, "an empty authorised tenant list matches nothing")
		otherTenant, err := q.QueryRange(ctx, prefix+`_calls_total{tenant_id="`+b+`"}`, end.Add(-5*time.Minute), end, time.Second, []string{a})
		require.NoError(t, err)
		assert.Len(t, otherTenant, 0, "extra_filters apply even when the query asks for another tenant")
		platform, err := q.QueryRange(ctx, "platform_"+prefix+"_calls_total", end.Add(-5*time.Minute), end, time.Second, []string{a})
		require.NoError(t, err)
		assert.Len(t, platform, 0, "unauthenticated observations cannot enter tenant panels")

		calls.IncContext(ctx, a)
		gauge.SetContext(WithTenant(ctx, a), 3, a)
		require.NoError(t, p.ForceFlush(ctx))
		awaitOTLPValue(ctx, t, q, prefix+"_calls_total", []string{a}, 6)
		awaitOTLPValue(ctx, t, q, prefix+"_facts_count", []string{a}, 3)
	})

	t.Run("replicas and restart", func(t *testing.T) {
		name := prefix + "_replica_calls_total"
		for _, item := range []struct {
			instance string
			value    float64
		}{
			{"replica-a", 6}, {"replica-b", 4}, {"replica-a", 2},
		} {
			r := NewRegistry()
			r.ScopedCounter(name, "Calls.", "tenant_id").AddContext(ctx, item.value, a)
			p, err := NewOTLP(ctx, config(r, item.instance))
			require.NoError(t, err)
			require.NoError(t, p.ForceFlush(ctx))
			require.NoError(t, p.Shutdown(ctx))
			// Give distinct cumulative streams and restarts distinct millisecond samples.
			time.Sleep(10 * time.Millisecond)
		}
		awaitOTLPValue(ctx, t, q, "sum(increase("+name+"[10m]))", []string{a}, 12)
		awaitOTLPValue(ctx, t, q, "sum("+name+")", []string{a}, 6)
		end := time.Now().Add(time.Minute)
		series, err := q.QueryRange(ctx, name, end.Add(-5*time.Minute), end, time.Second, []string{a})
		require.NoError(t, err)
		require.Len(t, series, 2, "replicas must remain separate cumulative streams")
		instances := map[string]bool{}
		for _, s := range series {
			instances[s.Labels["instance"]] = true
		}
		assert.Equal(t, instances, map[string]bool{"replica-a": true, "replica-b": true})
	})

	t.Run("bounded series have no SDK overflow", func(t *testing.T) {
		r := NewRegistry()
		name := prefix + "_bounded_total"
		c := r.ScopedCounter(name, "Bounded observations.", "tenant_id", "operation")
		c.budget = 3
		for i := range 6 {
			c.IncContext(ctx, a, fmt.Sprintf("operation_%d", i))
		}
		p, err := NewOTLP(ctx, config(r, "bounded"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, p.Shutdown(context.WithoutCancel(ctx))) })
		require.NoError(t, p.ForceFlush(ctx))
		awaitOTLPValue(ctx, t, q, "sum("+name+")", []string{a}, 3)
		got := awaitOTLPValue(ctx, t, q, name, nil, 1)
		require.Len(t, got, 3)
		for _, series := range got {
			assert.Equal(t, series.Labels["tenant_id"], a)
			for key := range series.Labels {
				assert.False(t, strings.Contains(key, "overflow"), "SDK must not create an unscoped overflow stream")
			}
		}
	})

	t.Run("receiver requires authentication", func(t *testing.T) {
		r := NewRegistry()
		r.ScopedCounter(prefix+"_rejected_total", "Rejected.", "tenant_id").IncContext(ctx, a)
		cfg := config(r, "bad-password")
		cfg.Password = "wrong-password"
		p, err := NewOTLP(ctx, cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Shutdown(context.WithoutCancel(ctx)) })
		require.Error(t, p.ForceFlush(ctx), "native OTLP receiver must reject wrong credentials")
	})
}

func awaitOTLPValue(ctx context.Context, t *testing.T, q *Querier, query string, tenants []string, want float64) []Series {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got []Series
	for {
		end := time.Now().Add(time.Minute)
		var err error
		got, err = q.QueryRange(ctx, query, end.Add(-5*time.Minute), end, time.Second, tenants)
		require.NoError(t, err)
		matched := len(got) > 0
		for _, s := range got {
			matched = matched && len(s.Points) > 0 && s.Points[len(s.Points)-1].V == want
		}
		if matched {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("query %q: want latest value %v, got %+v", query, want, got)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
