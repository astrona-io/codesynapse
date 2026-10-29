package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
)

// fakeVM answers query_range with one series and records the filters.
type fakeVM struct {
	mu      sync.Mutex
	filters []string
	queries []string
}

func (f *fakeVM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.filters = append(f.filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
	f.queries = append(f.queries, r.URL.Query().Get("query"))
	f.mu.Unlock()
	start := r.URL.Query().Get("start")
	_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"mcp_tool":"search"},"values":[[`+start+`,"3"]]}]}}`)
}

func (f *fakeVM) last() (filter, query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filters[len(f.filters)-1], f.queries[len(f.queries)-1]
}

func TestSeriesScopedByTenant(t *testing.T) {
	vm := &fakeVM{}
	vmSrv := httptest.NewServer(vm)
	defer vmSrv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, Metrics: &telemetry.Querier{URL: vmSrv.URL}}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer ts.Close()

	admin := newClient(t, ts)
	var adminSigned struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, admin.signUp(`{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, &adminSigned), 202)
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adminSigned.User.ID, true))
	bob := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 202)

	var p SeriesPanel
	require.Equal(t, bob.do("GET", "/api/v1/stats/series?panel=tool_calls&range=1h", "", &p), 200)
	filter, query := vm.last()
	assert.Equal(t, filter, `{tenant_id=~"`+signed.Org.ID+`"}`, "members see their own organisation only, enforced by VictoriaMetrics")
	assert.Contains(t, query, "[1m]", "the 1h range uses 1-minute steps")
	assert.Equal(t, len(p.Times), 60)
	require.Len(t, p.Series, 1)
	assert.Equal(t, p.Series[0].Name, "search")
	assert.Equal(t, p.Series[0].Total, 3.0)
	assert.True(t, p.Series[0].Values[len(p.Times)-1] != nil && *p.Series[0].Values[len(p.Times)-1] == 0, "count panels fill quiet steps with 0")

	for _, gone := range []string{"tenants", "platform_llm_cost", "platform_tool_latency", "platform_db_latency", "nope"} {
		assert.Equal(t, bob.do("GET", "/api/v1/stats/series?panel="+gone, "", nil), 404, gone)
		assert.Equal(t, admin.do("GET", "/api/v1/stats/series?panel="+gone, "", nil), 404, "cross-tenant panels moved to /admin/stats/series: "+gone)
	}
	assert.Equal(t, bob.do("GET", "/api/v1/stats/series?panel=tool_calls&range=2y", "", nil), 400)

	assert.Equal(t, bob.do("GET", "/api/v1/admin/stats/series?panel=org_tool_calls&range=7d", "", nil), 403, "cross-tenant panels are for platform admins")
	require.Equal(t, admin.do("GET", "/api/v1/admin/stats/series?panel=org_tool_calls&range=7d", "", &p), 200)
	filter, query = vm.last()
	assert.Equal(t, filter, "", "platform admins see every tenant")
	assert.Contains(t, query, "[1h]")

	require.Equal(t, admin.do("GET", "/api/v1/stats/series?panel=llm_cost", "", &p), 200)
	filter, _ = vm.last()
	assert.Equal(t, filter, `{tenant_id=~"`+adminSigned.Org.ID+`"}`, "ordinary panels never elevate platform users")
	var platformPanels []string
	for name := range platformHealthPanels {
		platformPanels = append(platformPanels, name)
	}
	for _, panel := range platformPanels {
		assert.Equal(t, bob.do("GET", "/api/v1/stats/series?panel="+panel, "", nil), 403)
		require.Equal(t, admin.do("GET", "/api/v1/stats/series?panel="+panel, "", nil), 200)
		filter, _ = vm.last()
		assert.Equal(t, filter, "")
	}
	for name, panel := range seriesPanels {
		if panel.platform || panel.repository {
			continue
		}
		require.Equal(t, bob.do("GET", "/api/v1/stats/series?panel="+name+"&tenant_id=other&query=up", "", &p), 200)
		filter, query = vm.last()
		assert.Equal(t, filter, `{tenant_id=~"`+signed.Org.ID+`"}`, name)
		assert.False(t, strings.Contains(query, "other"), "client filters cannot enter MetricsQL")
		assert.True(t, p.Empty != "", "each panel explains missing measurements")
		if p.Kind == "gauge" {
			assert.True(t, p.Times[len(p.Times)-1] <= time.Now().Unix(), "freshness is never evaluated against a future step")
		}
	}

	// The batch answers each panel as a request of its own would: scoped
	// to the caller, with each panel's status.
	var batch struct {
		Panels map[string]SeriesBatchItem
	}
	require.Equal(t, bob.do("GET", "/api/v1/stats/series/batch?panels=tool_calls,llm_cost,tool_calls,platform_api_requests,nope&range=1h", "", &batch), 200)
	require.Len(t, batch.Panels, 4, "duplicates count once")
	assert.Equal(t, batch.Panels["tool_calls"].Status, 200)
	require.NoError(t, json.Unmarshal(batch.Panels["tool_calls"].Body, &p))
	assert.Equal(t, p.Series[0].Total, 3.0)
	assert.Equal(t, len(p.Times), 60, "the range applies to every panel")
	assert.Equal(t, batch.Panels["llm_cost"].Status, 200)
	assert.Equal(t, batch.Panels["platform_api_requests"].Status, 403, "cross-tenant panels stay refused")
	assert.Equal(t, batch.Panels["nope"].Status, 404)
	filter, _ = vm.last()
	assert.Equal(t, filter, `{tenant_id=~"`+signed.Org.ID+`"}`)
	assert.Equal(t, bob.do("GET", "/api/v1/stats/series/batch?panels=", "", nil), 400)
	many := make([]string, maxBatchPanels+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	assert.Equal(t, bob.do("GET", "/api/v1/stats/series/batch?panels="+strings.Join(many, ","), "", nil), 400)

	plain := httptest.NewServer(New(ctx, Config{}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer plain.Close()
	assert.Equal(t, newClient(t, plain).do("GET", "/api/v1/stats/series?panel=tool_calls", "", nil), 409, "without VictoriaMetrics the panel says so")
}

func TestFoldSeries(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	var lines []SeriesLine
	for i, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		lines = append(lines, SeriesLine{Name: name, Total: float64(10 - i), Values: []*float64{v(float64(10 - i)), nil}})
	}
	got := foldSeries(lines, "count", 2)
	require.Len(t, got, maxNamedSeries+1)
	other := got[maxNamedSeries]
	assert.Equal(t, other.Name, "other")
	assert.Equal(t, other.Total, 6.0+5+4)
	assert.Equal(t, *other.Values[0], 15.0)
	assert.True(t, other.Values[1] == nil, "steps without data stay empty")
	assert.Len(t, foldSeries(lines[:5], "count", 2), maxNamedSeries+1, "a fifth series folds: hues never repeat")
	small := []SeriesLine{{Name: "b", Total: 9}, {Name: "a", Total: 1}}
	got = foldSeries(small, "count", 0)
	assert.Equal(t, got[0].Name, "a", "named series are ordered by name, not size, so colours stay with them")
	b, _ := json.Marshal(foldSeries(lines[:7], "gauge", 2))
	assert.False(t, strings.Contains(string(b), "other"), "levels are not summed")
}

func TestSeriesPlatformWithoutMembershipHasNoTenantData(t *testing.T) {
	vm := &fakeVM{}
	ts := httptest.NewServer(vm)
	defer ts.Close()
	s := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: ts.URL}}}
	ctx := context.WithValue(context.Background(), principalKey{}, accounts.Principal{User: accounts.User{PlatformAdmin: true}, Via: "session"})
	ctx = context.WithValue(ctx, scopeKey{}, scope{})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats/series?panel=memory_facts", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleSeries(w, r)
	require.Equal(t, w.Code, http.StatusOK)
	var out SeriesPanel
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Len(t, out.Series, 0)
	assert.Len(t, vm.queries, 0, "empty scope must not query VictoriaMetrics")
}

func TestSeriesQueryMeasurementSemantics(t *testing.T) {
	facts := seriesPanels["memory_facts"].queries[0].expr
	assert.Contains(t, facts, "max by (tenant_id)", "replicas are not summed")
	assert.Contains(t, facts, "== on (tenant_id) group_left", "only newest replica observations contribute")
	assert.Contains(t, facts, "time() - 90", "stale gauges disappear")
	assert.Contains(t, seriesPanels["cloud_savings"].queries[0].expr, "> 0", "zero denominators produce no result")
	assert.Contains(t, seriesPanels["db_rows"].queries[0].expr, "db_graph_rows_returned_total")
	assert.False(t, strings.Contains(seriesPanels["llm_cost"].Help, "free"))
}

func TestPhaseFivePanels(t *testing.T) {
	for _, tc := range []struct{ panel, contains, by string }{
		{"tool_errors", `mcp_tool_calls_total{outcome="error"}`, "mcp_tool"},
		{"tool_refusals", `mcp_tool_calls_total{outcome="refused"}`, "mcp_tool"},
		{"local_answers", `mcp_local_answers_total{outcome="recorded"}`, "mcp_tool"},
		{"llm_provider_failures", `llm_requests_total{outcome!="ok"}`, "llm_provider"},
		{"llm_failure_causes", `llm_requests_total{outcome!="ok"}`, "outcome"},
		{"gateway_latency", `gateway_request_duration_seconds_bucket`, "route"},
		{"gateway_fallthrough", `gateway_fallthrough_total`, "reason"},
	} {
		t.Run(tc.panel, func(t *testing.T) {
			p, ok := seriesPanels[tc.panel]
			require.True(t, ok)
			require.Len(t, p.queries, 1)
			assert.Contains(t, p.queries[0].expr, tc.contains)
			assert.Equal(t, p.queries[0].by, tc.by)
			assert.False(t, p.platform, "tenant panels stay scoped to the caller")
		})
	}
	assert.Contains(t, seriesPanels["tool_errors"].Help, "refused", "the help says refusals are counted apart")
}

// The gateway panels are fixed MetricsQL: the organisation panels take their
// tenant from extra_filters (TestSeriesScopedByTenant), and the platform
// twins run the same expressions across tenants for platform admins only.
func TestGatewayPanels(t *testing.T) {
	for _, tc := range []struct {
		panel    string
		contains []string
		by       string
	}{
		{"gateway_latency", []string{"histogram_quantile(0.95", "gateway_request_duration_seconds_bucket"}, "route"},
		{"gateway_latency_avg", []string{"gateway_request_duration_seconds_sum", "gateway_request_duration_seconds_count[$w])) > 0)"}, "route"},
		{"gateway_fallthrough", []string{"gateway_fallthrough_total"}, "reason"},
		{"gateway_timeouts", []string{`reason=~"queue_timeout|answer_timeout"`}, "reason"},
		{"gateway_queue_wait", []string{"gateway_queue_wait_seconds_sum", "gateway_queue_wait_seconds_bucket"}, ""},
	} {
		t.Run(tc.panel, func(t *testing.T) {
			p, ok := seriesPanels[tc.panel]
			require.True(t, ok)
			assert.False(t, p.platform, "organisation panels stay scoped to the caller")
			var all string
			for _, q := range p.queries {
				assert.Equal(t, q.by, tc.by)
				assert.False(t, strings.Contains(q.expr, "tenant_id"), "the tenant comes from extra_filters, never the query")
				all += q.expr
			}
			for _, c := range tc.contains {
				assert.Contains(t, all, c)
			}
		})
	}
	for _, name := range []string{"gateway_latency", "gateway_latency_avg", "gateway_fallthrough"} {
		a, ok := adminPanels[name]
		require.True(t, ok, name)
		assert.Equal(t, a.queries[0].expr, seriesPanels[name].queries[0].expr, "the platform panel shows the same series across tenants")
		assert.True(t, a.groups != "", "values few organisations use are folded")
	}
}

// The response-time quantile panels are four fixed histogram_quantile
// queries per route. The organisation panels never name a tenant (it comes
// from extra_filters); the platform twins run the same expressions and hide
// a route fewer than minGroupOrgs organisations used.
func TestGatewayQuantilePanels(t *testing.T) {
	want := []struct{ name, phi string }{{"p50", "0.5"}, {"p75", "0.75"}, {"p95", "0.95"}, {"p99", "0.99"}}
	for _, route := range []string{"local", "cloud"} {
		name := "gateway_latency_" + route
		t.Run(name, func(t *testing.T) {
			p, ok := seriesPanels[name]
			require.True(t, ok)
			assert.False(t, p.platform, "organisation panels stay scoped to the caller")
			assert.Equal(t, p.Kind, "gauge")
			assert.Equal(t, p.Unit, "seconds")
			assert.Contains(t, p.Help, "estimated from histogram buckets", "the tip says quantiles are bucket estimates")
			require.Len(t, p.queries, len(want))
			for i, q := range p.queries {
				assert.Equal(t, q.name, want[i].name)
				assert.Equal(t, q.by, "")
				assert.Equal(t, q.expr, `histogram_quantile(`+want[i].phi+`, sum by (le) (increase(gateway_request_duration_seconds_bucket{route="`+route+`"}[$w])))`)
				assert.False(t, strings.Contains(q.expr, "tenant_id"), "the tenant comes from extra_filters, never the query")
			}
			a, ok := adminPanels[name]
			require.True(t, ok)
			for i, q := range a.queries {
				assert.Equal(t, q.expr, p.queries[i].expr, "the platform panel shows the same series across tenants")
			}
			assert.Equal(t, a.groups, `count(sum by (tenant_id) (increase(gateway_request_duration_seconds_count{route="`+route+`"}[$w])) > 0)`)
		})
	}
	lines := []SeriesLine{{Name: "p50"}, {Name: "p99"}}
	assert.Equal(t, len(hideSmallPanel(lines, map[string]int{"": minGroupOrgs - 1})), 0, "a route few organisations used is not shown")
	assert.Equal(t, len(hideSmallPanel(lines, map[string]int{"": minGroupOrgs})), 2)
	assert.Equal(t, len(hideSmallPanel(lines, map[string]int{})), 0, "no count is no organisation")
}
