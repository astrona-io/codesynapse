package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func text(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, r.WriteText(&b))
	return b.String()
}

func TestCounterAndHistogramText(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("calls_total", "Calls.", "tenant_id", "tool")
	c.Inc("0123456789abcdef01234567", "search")
	c.Add(2, "0123456789abcdef01234567", "search")
	c.Add(-5, "0123456789abcdef01234567", "search") // ignored
	c.Inc("x")                                      // wrong arity: ignored
	h := r.Histogram("dur_seconds", "Duration.", []float64{0.1, 1}, "tool")
	h.Observe(0.05, "search")
	h.Observe(0.5, "search")
	h.Observe(5, "search")
	out := text(t, r)
	assert.Contains(t, out, "# TYPE calls_total counter\n")
	assert.Contains(t, out, `calls_total{tenant_id="0123456789abcdef01234567",tool="search"} 3`)
	assert.Contains(t, out, `dur_seconds_bucket{tool="search",le="0.1"} 1`)
	assert.Contains(t, out, `dur_seconds_bucket{tool="search",le="1"} 2`)
	assert.Contains(t, out, `dur_seconds_bucket{tool="search",le="+Inf"} 3`)
	assert.Contains(t, out, `dur_seconds_count{tool="search"} 3`)
}

func TestNewSeriesStartAtZero(t *testing.T) {
	nowMillis = func() int64 { return 1700000000000 }
	defer func() { nowMillis = func() int64 { return time.Now().UnixMilli() } }()
	r := NewRegistry()
	c := r.Counter("burst_total", "B.", "tool")
	c.Add(37809, "repo_list")
	first := text(t, r)
	assert.Contains(t, first, `burst_total{tool="repo_list"} 0 1700000000000`, "the first write starts the series at 0")
	assert.Contains(t, first, `burst_total{tool="repo_list"} 37809`)
	c.Add(1, "repo_list")
	assert.False(t, strings.Contains(text(t, r), " 0 1700000000000"), "only once")
	h := r.Histogram("lat_seconds", "L.", []float64{1}, "tool")
	h.Observe(0.5, "x")
	assert.Contains(t, text(t, r), `lat_seconds_count{tool="x"} 0 1700000000000`)
}

func TestLabelsStayBounded(t *testing.T) {
	assert.Equal(t, TenantLabel(""), NoTenant)
	assert.Equal(t, TenantLabel("0123456789abcdef01234567"), "0123456789abcdef01234567")
	assert.Equal(t, TenantLabel(`evil"} 1e9`), "invalid", "only organisation IDs pass")
	assert.Equal(t, Tenant(WithTenant(context.Background(), "0123456789abcdef01234567")), "0123456789abcdef01234567")
	assert.Equal(t, Tenant(context.Background()), NoTenant)

	AllowTools("search")
	assert.Equal(t, ToolLabel("search"), "search")
	assert.Equal(t, ToolLabel("DROP TABLE; anything a client sends"), "unknown")

	r := NewRegistry()
	c := r.Counter("q_total", "Q.", "v")
	c.Inc("line\nbreak \"quoted\" " + strings.Repeat("x", 200))
	out := text(t, r)
	assert.Contains(t, out, `q_total{v="linebreak \"quoted\" `)
	assert.False(t, strings.Contains(out, strings.Repeat("x", 65)), "values are capped at 64 bytes")
}

func TestSeriesCap(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("many_total", "Many.", "id")
	for i := range maxSeries + 10 {
		c.Inc(strings.Repeat("a", i%60) + string(rune('A'+i%26)) + strings.Repeat("b", i/1560))
	}
	c.mu.Lock()
	n := len(c.vals)
	c.mu.Unlock()
	assert.True(t, n <= maxSeries, "series are capped")
}

func TestPush(t *testing.T) {
	var got, auth, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, query = string(b), r.URL.RawQuery
		u, p, _ := r.BasicAuth()
		auth = u + ":" + p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := NewRegistry()
	r.Counter("pushed_total", "P.").Inc()
	p := &Pusher{URL: srv.URL, User: "cs", Password: "pw", Job: "api", Instance: "api-1", Registry: r}
	require.NoError(t, p.Push(context.Background()))
	assert.Contains(t, got, "pushed_total 1")
	assert.Equal(t, auth, "cs:pw")
	assert.Contains(t, query, "extra_label=job%3Dapi")
	assert.Error(t, CheckURL("http://user:pw@vm:8428"))
	assert.NoError(t, CheckURL("http://victoriametrics:8428"))
}
