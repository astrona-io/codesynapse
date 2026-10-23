package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestVictoriaMetrics pushes and queries a real VictoriaMetrics when
// CODESYNAPSE_TEST_METRICS_URL is set (just test-metrics).
func TestVictoriaMetrics(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_METRICS_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_METRICS_URL not set")
	}
	user, pass := os.Getenv("CODESYNAPSE_TEST_METRICS_USER"), os.Getenv("CODESYNAPSE_TEST_METRICS_PASSWORD")
	ctx := context.Background()
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"
	r := NewRegistry()
	c := r.Counter("cs_test_calls_total", "Test.", "tenant_id", "mcp_tool")
	c.Add(5, a, "search")
	c.Add(7, b, "search")
	p := &Pusher{URL: url, User: user, Password: pass, Job: "test", Instance: time.Now().Format("150405.000"), Registry: r}
	require.NoError(t, p.Push(ctx))
	time.Sleep(2 * time.Second)
	c.Add(1, a, "search")
	require.NoError(t, p.Push(ctx))

	q := &Querier{URL: url, User: user, Password: pass}
	got := awaitOTLPValue(ctx, t, q, `sum by (tenant_id) (max_over_time(cs_test_calls_total{job="test",instance="`+p.Instance+`"}[5m]))`, []string{a}, 6)
	require.Len(t, got, 1, "only tenant a's series comes back")
	assert.Equal(t, got[0].Labels["tenant_id"], a)
	assert.Equal(t, got[0].Points[len(got[0].Points)-1].V, 6.0)

	// A series whose first value is large (one call returning 37k tokens)
	// must count fully in increase(): the zero start sample makes it so.
	burst := r.Counter("cs_test_burst_total", "Burst.", "tenant_id")
	burst.Add(37809, a)
	require.NoError(t, p.Push(ctx))
	inc := awaitOTLPValue(ctx, t, q, `sum(increase(cs_test_burst_total{instance="`+p.Instance+`"}[10m]))`, nil, 37809)
	require.Len(t, inc, 1)
	assert.Equal(t, inc[0].Points[len(inc[0].Points)-1].V, 37809.0, "a large first value is an increase, not a pre-existing counter")

	none, err := q.QueryRange(ctx, `cs_test_calls_total`, time.Now().Add(-10*time.Minute), time.Now(), time.Minute, []string{})
	require.NoError(t, err)
	assert.Len(t, none, 0, "an empty tenant list matches nothing")
	_, err = q.QueryRange(ctx, `cs_test_calls_total`, time.Now().Add(-time.Minute), time.Now(), time.Minute, []string{`a"}|{x`})
	assert.ErrorIs(t, err, ErrBadTenant)
	bad := &Querier{URL: url, User: user, Password: "wrong"}
	_, err = bad.QueryRange(ctx, `up`, time.Now().Add(-time.Minute), time.Now(), time.Minute, nil)
	assert.Error(t, err, "VictoriaMetrics requires the password")
}
