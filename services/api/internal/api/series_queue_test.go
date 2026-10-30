package api

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// queueStep is one 15-minute step of a provider: requests in total, how
// many found a free slot, how many waited at most 1 s, 5 s and 30 s (at
// least the 5 s count when unset), and the summed wait.
type queueStep struct {
	end                              time.Time
	total, free, le1, le5, le30, sum float64
}

func queueSeries(provider string, steps ...queueStep) (buckets, sums []telemetry.Series) {
	bucket := func(le string, v func(queueStep) float64) telemetry.Series {
		s := telemetry.Series{Labels: map[string]string{"le": le, "llm_provider": provider}}
		for _, st := range steps {
			s.Points = append(s.Points, telemetry.Point{T: st.end.Unix(), V: v(st)})
		}
		return s
	}
	buckets = []telemetry.Series{
		bucket("0.005", func(s queueStep) float64 { return s.free }),
		bucket("1", func(s queueStep) float64 { return s.le1 }),
		bucket("5", func(s queueStep) float64 { return s.le5 }),
		bucket("30", func(s queueStep) float64 { return max(s.le30, s.le5) }),
		bucket("+Inf", func(s queueStep) float64 { return s.total }),
	}
	sum := telemetry.Series{Labels: map[string]string{"llm_provider": provider}}
	for _, st := range steps {
		sum.Points = append(sum.Points, telemetry.Point{T: st.end.Unix(), V: st.sum})
	}
	return buckets, []telemetry.Series{sum}
}

func slot(day, hour, minute int) int {
	return day*queueSlotsPerDay + (hour*60+minute)/queueSlotMinutes
}

func TestFoldQueueIntoWeekdaysAndSlots(t *testing.T) {
	cph, err := time.LoadLocation("Europe/Copenhagen")
	require.NoError(t, err)
	utc := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return at
	}
	type want struct {
		cell     int
		requests float64
		avg      float64 // -1: nil
	}
	cases := map[string]struct {
		loc   *time.Location
		steps []queueStep
		want  []want
		empty []int
	}{
		"utc": {
			loc:   time.UTC,
			steps: []queueStep{{end: utc("2026-10-26T09:15:00Z"), total: 4, free: 2, le1: 3, le5: 4, sum: 6}},
			want:  []want{{slot(0, 9, 0), 4, 1.5}},
			empty: []int{slot(0, 9, 15), slot(6, 23, 45)},
		},
		"the viewer's zone moves the step": {
			loc:   cph,
			steps: []queueStep{{end: utc("2026-10-26T09:15:00Z"), total: 4, free: 2, le1: 3, le5: 4, sum: 6}},
			want:  []want{{slot(0, 10, 0), 4, 1.5}},
			empty: []int{slot(0, 9, 0)},
		},
		"a step without requests is zero, not empty": {
			loc:   time.UTC,
			steps: []queueStep{{end: utc("2026-10-27T00:15:00Z")}},
			want:  []want{{slot(1, 0, 0), 0, -1}},
		},
		"the repeated hour of autumn lands twice in the same slots": {
			loc: cph,
			steps: []queueStep{
				{end: utc("2026-10-25T00:15:00Z"), total: 1, free: 1, le1: 1, le5: 1}, // 02:00 CEST
				{end: utc("2026-10-25T01:15:00Z"), total: 2, free: 2, le1: 2, le5: 2}, // 02:00 CET
			},
			want: []want{{slot(6, 2, 0), 3, 0}},
		},
		"the skipped hour of spring stays empty": {
			loc: cph,
			steps: []queueStep{
				{end: utc("2026-03-29T00:45:00Z"), total: 1, free: 1, le1: 1, le5: 1}, // 01:30 CET
				{end: utc("2026-03-29T01:15:00Z"), total: 1, free: 1, le1: 1, le5: 1}, // 03:00 CEST
			},
			want:  []want{{slot(6, 1, 30), 1, 0}, {slot(6, 3, 0), 1, 0}},
			empty: []int{slot(6, 2, 0), slot(6, 2, 15), slot(6, 2, 30), slot(6, 2, 45)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, s := queueSeries("local", tc.steps...)
			folded := foldQueue(b, s, 15*time.Minute, tc.loc)
			require.True(t, folded["local"] != nil)
			cells := folded["local"].publish()
			require.Len(t, cells, 7*96)
			for _, w := range tc.want {
				c := cells[w.cell]
				require.True(t, c != nil, "cell has measurements")
				assert.Equal(t, c.Requests, w.requests)
				if w.avg < 0 {
					assert.True(t, c.Avg == nil && c.P95 == nil && c.Share == nil, "no requests: no wait figures")
				} else {
					require.True(t, c.Avg != nil)
					assert.Equal(t, *c.Avg, w.avg)
				}
			}
			for _, i := range tc.empty {
				assert.True(t, cells[i] == nil, "a slot without measurements is empty")
			}
		})
	}
}

func TestQueueQuantileAndShare(t *testing.T) {
	a := queueAcc{seen: true, buckets: map[float64]float64{queueFirstBound: 10, 1: 15, 5: 19, math.Inf(1): 20}}
	p95, ok := a.quantile(0.95)
	require.True(t, ok)
	assert.True(t, math.Abs(p95-5) < 1e-9, "rank 19 ends in the 5 s bucket")
	p50, _ := a.quantile(0.5)
	assert.Equal(t, p50, queueFirstBound)
	share, ok := a.queued()
	require.True(t, ok)
	assert.Equal(t, share, 0.5)
	all := queueAcc{seen: true, buckets: map[float64]float64{queueFirstBound: 0, 1: 0, 5: 0, math.Inf(1): 4}}
	top, _ := all.quantile(0.95)
	assert.Equal(t, top, 5.0, "above the last bound reads as the last bound")
	_, ok = (&queueAcc{}).quantile(0.95)
	assert.False(t, ok)
}

func TestQueueVerdict(t *testing.T) {
	tue := func(h, m int) time.Time {
		return time.Date(2026, 10, 27, h, m, 0, 0, time.UTC).Add(15 * time.Minute)
	}
	cases := map[string]struct {
		steps []queueStep
		level string
		text  string
		first int
		last  int
	}{
		"too few": {
			steps: []queueStep{{end: tue(9, 0), total: 5, free: 5, le1: 5, le5: 5}},
			level: "unknown", text: "5 of the 20",
		},
		"full at peak": {
			steps: []queueStep{
				{end: tue(8, 45), total: 10, free: 10, le1: 10, le5: 10},
				{end: tue(9, 0), total: 10, free: 2, le1: 2, le5: 2, le30: 10, sum: 120},
				{end: tue(9, 15), total: 10, free: 3, le1: 3, le5: 3, le30: 10, sum: 120},
				{end: tue(9, 30), total: 10, free: 10, le1: 10, le5: 10},
			},
			level: "add_endpoint", text: "At peak (Tue 09:00–09:30) requests wait",
			first: slot(1, 9, 0) % queueSlotsPerDay, last: slot(1, 9, 15) % queueSlotsPerDay,
		},
		"some waiting": {
			steps: []queueStep{
				{end: tue(14, 0), total: 20, free: 14, le1: 20, le5: 20, sum: 3},
				{end: tue(15, 0), total: 20, free: 20, le1: 20, le5: 20},
			},
			level: "noticeable", text: "noticeable",
			first: slot(1, 14, 0) % queueSlotsPerDay, last: slot(1, 14, 0) % queueSlotsPerDay,
		},
		"never full": {
			steps: []queueStep{{end: tue(10, 0), total: 30, free: 30, le1: 30, le5: 30}},
			level: "fine", text: "one endpoint is enough",
			first: slot(1, 10, 0) % queueSlotsPerDay, last: slot(1, 10, 0) % queueSlotsPerDay,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, s := queueSeries("local", tc.steps...)
			v := foldQueue(b, s, 15*time.Minute, time.UTC)["local"].verdict()
			assert.Equal(t, v.Level, tc.level)
			assert.Contains(t, v.Text, tc.text)
			if tc.level != "unknown" {
				assert.Equal(t, v.Weekday, 1)
				assert.Equal(t, v.FirstSlot, tc.first)
				assert.Equal(t, v.LastSlot, tc.last)
			}
		})
	}
}

func TestViewerZone(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "UTC",
		"Europe/Copenhagen":      "Europe/Copenhagen",
		"America/St_Johns":       "America/St_Johns",
		"Local":                  "UTC",
		"../../etc/passwd":       "UTC",
		"Mars/Olympus":           "UTC",
		strings.Repeat("A", 100): "UTC",
	} {
		assert.Equal(t, viewerZone(in).String(), want, in)
	}
}

// queueVM answers every query with one bucket series and records the text
// and filters of the queue queries; the API's own background loops query
// other series meanwhile.
type queueVM struct {
	mu      sync.Mutex
	queries []string
	filters []string
}

func (f *queueVM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("query"); strings.Contains(q, "llm_") {
		f.mu.Lock()
		f.queries = append(f.queries, q)
		f.filters = append(f.filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
		f.mu.Unlock()
	}
	end := r.URL.Query().Get("end")
	_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"llm_provider":"local","le":"+Inf"},"values":[[`+end+`,"3"]]}]}}`)
}

func (f *queueVM) seen() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, fl := f.queries, f.filters
	f.queries, f.filters = nil, nil
	return q, fl
}

func TestQueuePeakIsScopedAndFixed(t *testing.T) {
	vm := &queueVM{}
	vmSrv := httptest.NewServer(vm)
	defer vmSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, Metrics: &telemetry.Querier{URL: vmSrv.URL}}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer ts.Close()

	admin := newClient(t, ts)
	var adminSigned struct{ User accounts.User }
	require.Equal(t, admin.signUp(`{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, &adminSigned), http.StatusAccepted)
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adminSigned.User.ID, true))
	bob := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), http.StatusAccepted)

	var peak QueuePeak
	require.Equal(t, bob.do("GET", "/api/v1/stats/queue?weeks=12&tz=Europe/Copenhagen&tenant_id=other", "", &peak), 200)
	queries, filters := vm.seen()
	assert.Equal(t, len(queries), 12*2+2, "two queries a week, then running and waiting")
	for i, q := range queries {
		assert.Equal(t, filters[i], `{tenant_id=~"`+signed.Org.ID+`"}`, "members see their own organisation only")
		for _, leak := range []string{"Europe", "Copenhagen", "other", signed.Org.ID, "platform_"} {
			assert.False(t, strings.Contains(q, leak), "the query text is fixed: "+q)
		}
	}
	assert.Equal(t, peak.Timezone, "Europe/Copenhagen")
	assert.Equal(t, peak.Weeks, 12)
	require.Len(t, peak.Providers, 1)
	assert.Equal(t, peak.Providers[0].Name, "local")
	assert.Len(t, peak.Providers[0].Cells, 7*96)
	assert.True(t, peak.Providers[0].Capacity == nil, "the installation's capacity is a platform figure")

	require.Equal(t, bob.do("GET", "/api/v1/stats/queue?tz=Nowhere/Else", "", &peak), 200)
	assert.Equal(t, peak.Timezone, "UTC")
	assert.Equal(t, peak.Weeks, 4)
	assert.Equal(t, bob.do("GET", "/api/v1/stats/queue?weeks=52", "", nil), 400)

	assert.Equal(t, bob.do("GET", "/api/v1/admin/stats/queue", "", nil), 403)
	vm.seen()
	require.Equal(t, admin.do("GET", "/api/v1/admin/stats/queue?weeks=1&tz=Asia/Kathmandu", "", &peak), 200)
	queries, filters = vm.seen()
	assert.Equal(t, len(queries), 2+3, "one week, then running, waiting and capacity")
	for i, q := range queries {
		assert.Equal(t, filters[i], "", "platform admins see every tenant")
		assert.False(t, strings.Contains(q, "Kathmandu"), q)
	}
	assert.Equal(t, peak.Scope, "platform")
	assert.True(t, peak.Providers[0].Capacity != nil)
}

func TestQueuePanelsAreFixed(t *testing.T) {
	for _, set := range []map[string]seriesPanel{queuePanels(orgQueue, false), queuePanels(platformQueue, true)} {
		for name, p := range set {
			for _, q := range p.queries {
				assert.True(t, strings.Contains(q.expr, "$w"), name)
				assert.False(t, strings.Contains(q.expr, "tenant_id"), "the tenant is an extra filter: "+name)
			}
		}
	}
	for name, p := range queuePanels(orgQueue, false) {
		for _, q := range p.queries {
			assert.False(t, strings.Contains(q.expr, "platform_"), "organisations never read installation series: "+name)
		}
		_, ok := seriesPanels[name]
		assert.True(t, ok, name)
		_, ok = adminPanels[name]
		assert.True(t, ok, name)
	}
}

// A model's tab asks for its queue charts with ?provider=: only configured
// providers pass, and the name travels in the tenant's extra_filters
// selector, never in the query text.
func TestQueueSeriesByProvider(t *testing.T) {
	vm := &queueVM{}
	vmSrv := httptest.NewServer(vm)
	defer vmSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:1/v1", Model: "qwen", EmbeddingModel: "nomic", MaxConcurrent: 1, TimeoutSeconds: 5},
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", TimeoutSeconds: 5},
	}, Default: "local"})
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, Metrics: &telemetry.Querier{URL: vmSrv.URL}, LLM: reg}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer ts.Close()

	admin := newClient(t, ts)
	var adminSigned struct{ User accounts.User }
	require.Equal(t, admin.signUp(`{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, &adminSigned), http.StatusAccepted)
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adminSigned.User.ID, true))
	bob := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), http.StatusAccepted)
	tenant := `tenant_id=~"` + signed.Org.ID + `"`

	cases := map[string]struct {
		client *client
		path   string
		status int
		filter string // every recorded query's extra_filters; "-" when none may run
	}{
		"a configured provider":         {bob, "/api/v1/stats/series?panel=queue_wait&range=1h&provider=local", 200, `{` + tenant + `,llm_provider="local"}`},
		"without a provider":            {bob, "/api/v1/stats/series?panel=queue_wait&range=1h", 200, `{` + tenant + `}`},
		"an unknown provider":           {bob, "/api/v1/stats/series?panel=queue_wait&range=1h&provider=nope", 400, "-"},
		"a matcher in the name":         {bob, "/api/v1/stats/series?panel=queue_wait&range=1h&provider=" + url.QueryEscape(`local",tenant_id=~".*`), 400, "-"},
		"a panel not split by provider": {bob, "/api/v1/stats/series?panel=llm_cost&range=1h&provider=local", 400, "-"},
		"the batch passes it on":        {bob, "/api/v1/stats/series/batch?panels=queue_load,queue_start&range=1h&provider=cloud", 200, `{` + tenant + `,llm_provider="cloud"}`},
		"platform, every tenant":        {admin, "/api/v1/admin/stats/series?panel=queue_saturation&range=1h&provider=local", 200, `{llm_provider="local"}`},
		"platform, unknown provider":    {admin, "/api/v1/admin/stats/series?panel=queue_saturation&range=1h&provider=nope", 400, "-"},
		"platform batch":                {admin, "/api/v1/admin/stats/series/batch?panels=queue_load,queue_wait&range=1h&provider=local", 200, `{llm_provider="local"}`},
		"platform batch, unknown":       {admin, "/api/v1/admin/stats/series/batch?panels=queue_load&range=1h&provider=nope", 400, "-"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			vm.seen()
			assert.Equal(t, tc.client.do("GET", tc.path, "", nil), tc.status)
			queries, filters := vm.seen()
			if tc.filter == "-" {
				assert.Len(t, queries, 0, "nothing reaches VictoriaMetrics")
				return
			}
			require.True(t, len(queries) > 0)
			for i, q := range queries {
				assert.Equal(t, filters[i], tc.filter)
				for _, leak := range []string{`"local"`, `"cloud"`, "tenant_id"} {
					assert.False(t, strings.Contains(q, leak), "the query text is fixed: "+q)
				}
			}
		})
	}

	var batch struct{ Panels map[string]SeriesBatchItem }
	require.Equal(t, admin.do("GET", "/api/v1/admin/stats/series/batch?panels=queue_load,platform_db_latency&range=1h&provider=local", "", &batch), 200)
	assert.Equal(t, batch.Panels["queue_load"].Status, 200)
	assert.Equal(t, batch.Panels["platform_db_latency"].Status, 404)

	var peak QueuePeak
	require.Equal(t, bob.do("GET", "/api/v1/stats/queue?weeks=1", "", &peak), 200)
	require.Len(t, peak.Providers, 1, "the configured provider with a queue; cloud has no limit and no requests")
	p := peak.Providers[0]
	assert.Equal(t, p.Name, "local")
	assert.True(t, p.Configured)
	assert.Equal(t, p.Models, []string{"qwen", "nomic"})
	assert.Equal(t, p.Slots, 1)
	assert.True(t, p.P95 != nil, "the fake answers three requests")
}
