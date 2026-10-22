package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
	var got []Series
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		end := time.Now().Add(time.Minute)
		var err error
		got, err = q.QueryRange(ctx, `sum by (tenant_id) (max_over_time(cs_test_calls_total{job="test",instance="`+p.Instance+`"}[5m]))`, end.Add(-10*time.Minute), end, time.Minute, []string{a})
		require.NoError(t, err)
		if len(got) > 0 && len(got[0].Points) > 0 && got[0].Points[len(got[0].Points)-1].V == 6 {
			break
		}
		time.Sleep(time.Second)
	}
	require.Len(t, got, 1, "only tenant a's series comes back")
	assert.Equal(t, got[0].Labels["tenant_id"], a)
	assert.Equal(t, got[0].Points[len(got[0].Points)-1].V, 6.0)

	// A series whose first value is large (one call returning 37k tokens)
	// must count fully in increase(): the zero start sample makes it so.
	burst := r.Counter("cs_test_burst_total", "Burst.", "tenant_id")
	burst.Add(37809, a)
	require.NoError(t, p.Push(ctx))
	var inc []Series
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		end := time.Now().Add(time.Minute)
		var err error
		inc, err = q.QueryRange(ctx, `sum(increase(cs_test_burst_total{instance="`+p.Instance+`"}[10m]))`, end.Add(-5*time.Minute), end, time.Minute, nil)
		require.NoError(t, err)
		if len(inc) > 0 && len(inc[0].Points) > 0 && inc[0].Points[len(inc[0].Points)-1].V == 37809 {
			break
		}
		time.Sleep(time.Second)
	}
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
