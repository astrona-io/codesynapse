package usage

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLatencyIndex(t *testing.T) {
	for _, tc := range []struct{ ms, want int }{
		{0, 0}, {50, 0}, {51, 1}, {100, 1}, {999, 4}, {1000, 4}, {150000, 10}, {150001, 11},
	} {
		assert.Equal(t, latencyIndex(tc.ms), tc.want, "%d ms", tc.ms)
	}
	assert.Equal(t, len(LatencyColumns()), LatencyBuckets)
}

func TestAddKeepsMinimumOfTimedCalls(t *testing.T) {
	var b Bucket
	b.Add(Bucket{Calls: 1, RangesReturned: 3}) // untimed: no minimum yet
	b.Observe(300, OK)
	b.Observe(900, TimedOut)
	b.Add(Bucket{Calls: 1, MinDurationMs: 0}) // an untimed 0 never wins
	assert.Equal(t, b.MinDurationMs, 300)
	assert.Equal(t, b.MaxDurationMs, 900)
	assert.Equal(t, b.Errors, 1)
	assert.Equal(t, b.Timeouts, 1)
	assert.Equal(t, b.Latency.Count(), 2)
	o := Bucket{}
	o.Observe(20, Failed)
	b.Add(o)
	assert.Equal(t, b.MinDurationMs, 20)
	assert.Equal(t, b.Timeouts, 1, "only timeouts count as timeouts")
}

func TestQuantile(t *testing.T) {
	var none Latency
	assert.Equal(t, none.Quantile(0.95, 0, 0), 0)

	var b Bucket
	for range 19 {
		b.Observe(80, OK) // (50, 100]
	}
	b.Observe(20000, OK) // (10000, 30000]
	h := b.Latency
	p50 := h.Quantile(0.5, b.MinDurationMs, b.MaxDurationMs)
	assert.True(t, p50 > 50 && p50 <= 100, "p50 %d inside its bucket", p50)
	assert.Equal(t, h.Quantile(1, b.MinDurationMs, b.MaxDurationMs), 20000, "clamped to the maximum")
	assert.True(t, h.Quantile(0.95, b.MinDurationMs, b.MaxDurationMs) <= 100, "19 of 20 are fast")

	var one Bucket
	one.Observe(30, OK)
	assert.Equal(t, one.Latency.Quantile(0.5, one.MinDurationMs, one.MaxDurationMs), 30, "clamped to the observed range")
	var slow Bucket
	slow.Observe(400000, OK)
	assert.Equal(t, slow.Latency.Quantile(0.95, slow.MinDurationMs, slow.MaxDurationMs), 400000, "overflow reaches the maximum")
}

func TestParseOutcome(t *testing.T) {
	for in, want := range map[string]Outcome{"": OK, "ok": OK, "error": Failed, "timeout": TimedOut} {
		got, err := ParseOutcome(in)
		require.NoError(t, err)
		assert.Equal(t, got, want)
	}
	_, err := ParseOutcome("slow")
	require.Error(t, err)
}

func TestSpreadAndStart(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 15, 0, 0, time.UTC)
	got := Spread(Bucket{Tool: "x", Calls: 1}, at)
	require.Len(t, got, 2)
	assert.Equal(t, got[0].Day, "2026-11-02")
	assert.Equal(t, got[0].Hour, "")
	assert.Equal(t, got[1].Hour, "09")
	assert.True(t, got[0].Key() != got[1].Key(), "day and hour buckets differ")
	start, err := got[1].Start()
	require.NoError(t, err)
	assert.Equal(t, start, time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
}

func TestFeatures(t *testing.T) {
	assert.True(t, IsWorkflow(FeaturePreBrief), "features stay out of tool statistics")
	assert.True(t, Timed(FeaturePreBrief), "but are timed")
	assert.False(t, Timed(Session))
	assert.False(t, Timed(FirstNative))
	assert.True(t, Timed("search"))
}

func TestReportedFeature(t *testing.T) {
	f, ok := ReportedFeature("pre-brief")
	assert.True(t, ok)
	assert.Equal(t, f, FeaturePreBrief)
	_, ok = ReportedFeature("gateway-local")
	assert.False(t, ok, "the server times the gateway itself")
	_, ok = ReportedFeature("anything")
	assert.False(t, ok)
}
