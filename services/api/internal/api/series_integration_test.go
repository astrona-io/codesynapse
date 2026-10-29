package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestVictoriaMetricsDashboardPanels(t *testing.T) {
	base := os.Getenv("CODESYNAPSE_TEST_METRICS_URL")
	if base == "" {
		t.Skip("CODESYNAPSE_TEST_METRICS_URL not set")
	}
	const a, b, c, d = "111111111111111111111111", "222222222222222222222222", "333333333333333333333333", "444444444444444444444444"
	const e, f = "555555555555555555555555", "666666666666666666666666"
	ctx := t.Context()
	now := time.Now().Truncate(time.Second)
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	addLabels := func(name, tenant, replica string, values []float64, times []int64, extra ...string) {
		t.Helper()
		labels := map[string]string{"__name__": name, "tenant_id": tenant, "instance": replica, "mcp_tool": "none", "llm_provider": "none", "llm_model": "none"}
		for i := 0; i+1 < len(extra); i += 2 {
			labels[extra[i]] = extra[i+1]
		}
		require.NoError(t, enc.Encode(struct {
			Metric map[string]string `json:"metric"`
			Values []float64         `json:"values"`
			Times  []int64           `json:"timestamps"`
		}{labels, values, times}))
	}
	add := func(name, tenant, replica string, values []float64, times []int64) {
		t.Helper()
		addLabels(name, tenant, replica, values, times)
	}
	gauge := func(tenant, replica string, count float64, age time.Duration) {
		at := now.Add(-age)
		add("agent_memory_facts_count", tenant, replica, []float64{count}, []int64{at.UnixMilli()})
		add("agent_memory_facts_observed_at_seconds", tenant, replica, []float64{float64(at.Unix())}, []int64{at.UnixMilli()})
	}
	gauge(a, "old", 100, 40*time.Second)
	gauge(a, "new", 5, 10*time.Second)
	gauge(a, "new-duplicate", 5, 10*time.Second)
	gauge(b, "new", 10, 10*time.Second)
	gauge(c, "stale", 999, 200*time.Second)
	// Model a failed count batch followed by a successful freshness batch,
	// and the reverse. Neither partial publication is a complete observation.
	partial := func(tenant, replica string, countAge, freshnessAge time.Duration) {
		at := now.Add(-freshnessAge)
		add("agent_memory_facts_count", tenant, replica, []float64{999}, []int64{now.Add(-countAge).UnixMilli()})
		add("agent_memory_facts_observed_at_seconds", tenant, replica, []float64{float64(at.Unix())}, []int64{at.UnixMilli()})
	}
	partial(e, "count-export-failed", 30*time.Second, 2*time.Second)
	partial(f, "freshness-export-failed", 2*time.Second, 30*time.Second)
	partial(a, "partial-newer-replica", 30*time.Second, 2*time.Second)
	counter := func(name, tenant string, n float64) {
		add(name, tenant, "one", []float64{0, n}, []int64{now.Add(-2 * time.Minute).UnixMilli(), now.Add(-20 * time.Second).UnixMilli()})
	}
	for tenant, amounts := range map[string][2]float64{a: {300, 100}, b: {100, 900}, c: {0, 0}} {
		counter("cloud_bytes_sent_total", tenant, amounts[0])
		counter("cloud_bytes_saved_total", tenant, amounts[1])
	}
	counter("cloud_bytes_sent_total", d, 100)
	counter("llm_cost_estimated_dollars", a, 0.000004)
	counter("llm_cost_estimated_dollars", b, 9)
	// Gateway response time buckets: tenant a's local answers all took at
	// most 1 s, tenant b's between 1 and 2.5 s. The quantile panel scoped to
	// a must not see b's buckets.
	bucket := func(tenant, route, le string, n float64) {
		addLabels("gateway_request_duration_seconds_bucket", tenant, "one", []float64{0, n}, []int64{now.Add(-2 * time.Minute).UnixMilli(), now.Add(-20 * time.Second).UnixMilli()}, "route", route, "le", le)
	}
	for _, le := range []string{"1", "2.5", "+Inf"} {
		bucket(a, "local", le, 10)
		bucket(b, "cloud", le, 10)
	}
	for le, n := range map[string]float64{"1": 0, "2.5": 10, "+Inf": 10} {
		bucket(b, "local", le, n)
	}
	user, pass := os.Getenv("CODESYNAPSE_TEST_METRICS_USER"), os.Getenv("CODESYNAPSE_TEST_METRICS_PASSWORD")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/v1/import", &body)
	require.NoError(t, err)
	req.SetBasicAuth(user, pass)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, res.Body)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())
	require.Equal(t, res.StatusCode, http.StatusNoContent)
	q := &telemetry.Querier{URL: base, User: user, Password: pass}
	query := func(panel string, tenants []string) []telemetry.Series {
		t.Helper()
		expr := strings.ReplaceAll(seriesPanels[panel].queries[0].expr, "$w", "5m")
		got, err := q.QueryRange(ctx, expr, now, now, time.Minute, tenants)
		require.NoError(t, err)
		return got
	}
	// Imports become queryable asynchronously. Avoid caching a negative result.
	deadline := time.Now().Add(20 * time.Second)
	for len(query("memory_facts", []string{a})) == 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.Fatal(context.Cause(ctx))
		case <-time.After(100 * time.Millisecond):
		}
	}
	for _, tc := range []struct {
		name, panel string
		tenants     []string
		want        float64
	}{
		{"latest replica after deletion", "memory_facts", []string{a}, 5},
		{"sum tenants after replica deduplication", "memory_facts", []string{a, b}, 15},
		{"both ratio sides tenant scoped", "cloud_savings", []string{a}, 25},
		{"other tenant ratio", "cloud_savings", []string{b}, 90},
		{"small nonzero cost", "llm_cost", []string{a}, 0.000004},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := query(tc.panel, tc.tenants)
			require.Len(t, got, 1)
			require.Len(t, got[0].Points, 1)
			assert.Equal(t, got[0].Points[0].V, tc.want)
		})
	}
	for _, tc := range []struct {
		name    string
		tenants []string
		lo, hi  float64
	}{
		{"own buckets only", []string{a}, 0, 1},
		{"the other tenant's buckets", []string{b}, 1, 2.5},
	} {
		t.Run("gateway p50 "+tc.name, func(t *testing.T) {
			got := query("gateway_latency_local", tc.tenants)
			require.Len(t, got, 1)
			require.Len(t, got[0].Points, 1)
			v := got[0].Points[0].V
			assert.True(t, v > tc.lo && v <= tc.hi, "p50 from the tenant's own buckets")
		})
	}
	assert.Len(t, query("gateway_latency_cloud", []string{a}), 0, "a route the tenant never used is absent")
	assert.Len(t, query("memory_facts", []string{c}), 0, "stale gauges are absent")
	assert.Len(t, query("memory_facts", []string{e}), 0, "a fresh timestamp must not refresh a count whose export failed")
	assert.Len(t, query("memory_facts", []string{f}), 0, "a new count without its matching freshness sample is incomplete")
	assert.Len(t, query("cloud_savings", []string{c}), 0, "zero comparable traffic is absent")
	assert.Len(t, query("cloud_savings", []string{d}), 0, "missing fallback baseline is absent")
	assert.Len(t, query("memory_facts", []string{}), 0, "no organisation means no data")
	for _, panel := range seriesPanels {
		for _, query := range panel.queries {
			_, err := q.QueryRange(ctx, strings.ReplaceAll(query.expr, "$w", "5m"), now, now, time.Minute, []string{a})
			require.NoError(t, err, "all fixed panel expressions parse")
		}
	}
}
