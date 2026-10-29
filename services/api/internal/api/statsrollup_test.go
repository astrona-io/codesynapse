package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestNextStatsRollup(t *testing.T) {
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "midnight", now: day, want: 5 * time.Minute},
		{name: "just before", now: day.Add(4 * time.Minute), want: time.Minute},
		{name: "at the time", now: day.Add(5 * time.Minute), want: 24 * time.Hour},
		{name: "afternoon", now: day.Add(14 * time.Hour), want: 10*time.Hour + 5*time.Minute},
		{name: "another zone", now: day.Add(23 * time.Hour).In(time.FixedZone("CET", 3600)), want: time.Hour + 5*time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, nextStatsRollup(c.now), c.want)
		})
	}
}

func TestGatewayStatSource(t *testing.T) {
	q := gatewayRollupQueries
	series := map[string]string{
		q.requests:     `{"tenant_id":"org-a","route":"local","class":"2xx"}=6;{"tenant_id":"org-a","route":"cloud","class":"2xx"}=3;{"tenant_id":"org-b","route":"cloud","class":"5xx"}=1;{"tenant_id":"org-b","route":"rejected","class":"4xx"}=2`,
		q.fallthroughs: `{"tenant_id":"org-a","reason":"queue_timeout"}=2;{"tenant_id":"org-a","reason":"too_large"}=1;{"tenant_id":"org-b","reason":"answer_timeout"}=1;{"tenant_id":"org-b","reason":"made_up"}=1`,
		q.latencySum:   `{"tenant_id":"org-a","route":"local"}=12.5;{"tenant_id":"org-a","route":"cloud"}=NaN;{"tenant_id":"org-b","route":"rejected"}=1`,
		q.latencyCount: `{"tenant_id":"org-a","route":"local"}=6;{"tenant_id":"org-a","route":"cloud"}=3`,
		q.queueSum:     `{"tenant_id":"org-a"}=0.75`,
		q.queueCount:   `{"tenant_id":"org-a"}=6`,
	}
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var filters, ends []string
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filters = append(filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
		ends = append(ends, r.URL.Query().Get("end"))
		var result []string
		for _, part := range strings.Split(series[r.URL.Query().Get("query")], ";") {
			if labels, value, ok := strings.Cut(part, "="); ok {
				result = append(result, fmt.Sprintf(`{"metric":%s,"values":[[%s,"%s"]]}`, labels, r.URL.Query().Get("end"), value))
			}
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[`+strings.Join(result, ",")+`]}}`)
	}))
	defer vm.Close()
	srv := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: vm.URL}}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	set := accounts.NewPlatformStatSet(day)
	require.NoError(t, srv.gatewayStatSource(context.Background(), day, set))
	got := map[string]int64{}
	for _, r := range set.Rows() {
		if r.Metric == accounts.StatGateway && r.Value != 0 {
			got[r.OrgID+"/"+r.Dimension] = r.Value
		}
	}
	assert.Equal(t, got, map[string]int64{
		"/requests": 12, "/local": 6, "/cloud": 4, "/rejected": 2, "/failed": 3, "/fallthroughs": 5, "/queue_timeout": 2, "/answer_timeout": 1,
		"/local_ms": 12500, "/local_n": 6, "/cloud_n": 3, "/queue_ms": 750, "/queue_n": 6, "/measured": 1,
		"org-a/requests": 9, "org-a/local": 6, "org-a/cloud": 3, "org-a/fallthroughs": 3, "org-a/queue_timeout": 2,
		"org-a/local_ms": 12500, "org-a/local_n": 6, "org-a/cloud_n": 3, "org-a/queue_ms": 750, "org-a/queue_n": 6,
		"org-b/requests": 3, "org-b/cloud": 1, "org-b/rejected": 2, "org-b/failed": 3, "org-b/fallthroughs": 2, "org-b/answer_timeout": 1,
	})
	for i, f := range filters {
		assert.Equal(t, f, "", "the rollup is platform work: no tenant filter")
		assert.Equal(t, ends[i], fmt.Sprint(day.AddDate(0, 0, 1).Unix()), "evaluated at the end of the day")
	}

	t.Run("metrics store down", func(t *testing.T) {
		down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
		defer down.Close()
		srv := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: down.URL}}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		set := accounts.NewPlatformStatSet(day)
		require.NoError(t, srv.gatewayStatSource(context.Background(), day, set), "the accounts figures are written anyway")
		for _, r := range set.Rows() {
			if r.Metric == accounts.StatGateway {
				assert.Equal(t, r.Value, int64(0), r.Dimension+" stays zero, measured 0")
			}
		}
	})
}
