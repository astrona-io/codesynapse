package api

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

func TestFeatureHealthFromHookTimings(t *testing.T) {
	ts, _ := newTestServer(t)
	path := "/api/v1/repos/" + storetest.Repo + "/usage/retrieval"
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","timings":[
		{"feature":"pre-brief","duration_ms":400},
		{"feature":"pre-brief","duration_ms":9000,"outcome":"timeout"},
		{"feature":"pre-brief","duration_ms":700,"outcome":"error"},
		{"feature":"stop","duration_ms":30}]}`), 204, "timings alone are a report")

	var st FeatureHealthStats
	require.Equal(t, get(t, ts, "/api/v1/stats/features?window=1h", &st), 200)
	assert.Equal(t, st.Window, "1h")
	assert.Equal(t, len(st.Bounds), len(usage.LatencyBounds))
	require.Len(t, st.Features, 2)
	pb := st.Features[0]
	assert.Equal(t, pb.Feature, "pre-brief", "slowest p95 first")
	assert.Equal(t, pb.Kind, "model")
	assert.Equal(t, pb.Total.Calls, 3)
	assert.Equal(t, pb.Total.Errors, 2)
	assert.Equal(t, pb.Total.Timeouts, 1)
	assert.Equal(t, pb.Total.MinMs, 400)
	assert.Equal(t, pb.Total.MaxMs, 9000)
	assert.Equal(t, pb.Total.AvgMs, 3366)
	require.Len(t, pb.Clients, 1)
	assert.Equal(t, pb.Clients[0].Client, "claude-code")
	assert.Equal(t, st.Features[1].Feature, "stop")
	assert.Equal(t, st.Features[1].Kind, "hook")

	assert.Equal(t, get(t, ts, "/api/v1/stats/features?window=2d", nil), 400)
	require.Equal(t, get(t, ts, "/api/v1/stats/features", &st), 200)
	assert.Equal(t, st.Window, "24h", "the default window")

	for _, body := range []string{
		`{"client":"claude-code","timings":[{"feature":"rm_rf","duration_ms":1}]}`,
		`{"client":"claude-code","timings":[{"feature":"search","duration_ms":1}]}`,
		`{"client":"claude-code","timings":[{"feature":"stop","duration_ms":-1}]}`,
		`{"client":"claude-code","timings":[{"feature":"stop","duration_ms":1,"outcome":"slow"}]}`,
		`{"client":"claude-code","timings":[{"feature":"stop","duration_ms":1,"path":"a.go"}]}`,
	} {
		assert.Equal(t, post(t, ts, path, body), 400, body)
	}
}

func TestFeatureHealthKeepsTheWindow(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration, ms int, o usage.Outcome) []usage.Bucket {
		b := usage.Bucket{Tool: "ask_local", Client: "claude-code"}
		b.Observe(ms, o)
		return usage.Spread(b, now.Add(-d))
	}
	var buckets []usage.Bucket
	buckets = append(buckets, at(0, 100, usage.OK)...)
	buckets = append(buckets, at(3*time.Hour, 5000, usage.TimedOut)...)
	buckets = append(buckets, usage.Bucket{Day: usage.Day(now), Hour: now.Format("15"), Tool: usage.FirstNative, Calls: 4})
	since := now.Truncate(time.Hour).Add(-time.Hour)
	got := featureHealth(buckets, since)
	require.Len(t, got, 1, "day buckets and untimed workflow counts are left out")
	assert.Equal(t, got[0].Total.Calls, 1, "the older hour is outside the window")
	assert.Equal(t, got[0].Total.Timeouts, 0)

	got = featureHealth(buckets, now.Add(-24*time.Hour))
	require.Len(t, got, 1)
	assert.Equal(t, got[0].Total.Calls, 2)
	assert.Equal(t, got[0].Total.Timeouts, 1)
	assert.Equal(t, got[0].Total.MinMs, 100)
	assert.Equal(t, got[0].Total.MaxMs, 5000)
	assert.Equal(t, got[0].Kind, "tool")
}

func TestGatewayLocalTiming(t *testing.T) {
	for name, tc := range map[string]struct {
		m    gateway.Meta
		d    time.Duration
		o    usage.Outcome
		seen bool
	}{
		"not asked":   {m: gateway.Meta{}, seen: false},
		"cancelled":   {m: gateway.Meta{Asked: true, Fallthrough: gateway.FallCancelled}, seen: false},
		"answered":    {m: gateway.Meta{Asked: true, Route: "local", QueueWait: time.Second, LocalAnswer: 2 * time.Second}, d: 3 * time.Second, o: usage.OK, seen: true},
		"streamed":    {m: gateway.Meta{Asked: true, Route: "local", Stream: true, FirstByte: 800 * time.Millisecond, LocalAnswer: 5 * time.Second}, d: 800 * time.Millisecond, o: usage.OK, seen: true},
		"timed out":   {m: gateway.Meta{Asked: true, Fallthrough: gateway.FallTimeout, LocalAnswer: 10 * time.Second}, d: 10 * time.Second, o: usage.TimedOut, seen: true},
		"queue":       {m: gateway.Meta{Asked: true, Fallthrough: gateway.FallQueueTimeout, QueueWait: time.Second}, d: time.Second, o: usage.TimedOut, seen: true},
		"model error": {m: gateway.Meta{Asked: true, Fallthrough: gateway.FallModelError, LocalAnswer: time.Second}, d: time.Second, o: usage.Failed, seen: true},
	} {
		t.Run(name, func(t *testing.T) {
			d, o, seen := gatewayLocalTiming(tc.m)
			assert.Equal(t, seen, tc.seen)
			if tc.seen {
				assert.Equal(t, d, tc.d)
				assert.Equal(t, o, tc.o)
			}
		})
	}
}

// A local call that ran out of time is a timeout and an error, and a
// fetch_source extract is timed as the source-extract feature.
func TestLocalUsageTimings(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/usage/local"
	h := runtimeHeaders("thread-a")
	require.Equal(t, f.client.do("POST", path, `{"tool":"ask_local","client":"claude-code","repo":"`+storetest.Repo+`","duration_ms":60000,"failed":true,"timed_out":true}`, nil, h...), 204)
	require.Equal(t, f.client.do("POST", path, `{"tool":"fetch_source","client":"claude-code","duration_ms":40,"source":{"result":"miss","raw_tokens":10,"extract_us":2000000,"extract_outcome":"error"}}`, nil, h...), 204)
	assert.Equal(t, f.client.do("POST", path, `{"tool":"fetch_source","client":"claude-code","source":{"result":"miss","extract_us":1,"extract_outcome":"slow"}}`, nil, h...), 400)

	hours, err := f.store.UsageHours(t.Context(), "")
	require.NoError(t, err)
	got := map[string]usage.Bucket{}
	for _, b := range hours {
		got[b.Tool] = b
	}
	ask := got["ask_local"]
	assert.Equal(t, ask.Calls, 1)
	assert.Equal(t, ask.Errors, 1)
	assert.Equal(t, ask.Timeouts, 1)
	assert.Equal(t, ask.MaxDurationMs, 60000)
	x := got[usage.FeatureSourceExtract]
	assert.Equal(t, x.Calls, 1)
	assert.Equal(t, x.Errors, 1)
	assert.Equal(t, x.MinDurationMs, 2000)
	assert.Equal(t, x.Repo, "")
}
