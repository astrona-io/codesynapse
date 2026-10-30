package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// cannedVM answers each fixed query (the window written back as $r) with
// canned series "{labels}=value;…" and records the extra_filters it got.
func cannedVM(t *testing.T, series map[string]string, filters *[]string) *httptest.Server {
	t.Helper()
	window := regexp.MustCompile(`\[(1h|6h|24h|7d|30d)\]`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*filters = append(*filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
		query := window.ReplaceAllString(r.URL.Query().Get("query"), "[$$r]")
		var result []string
		for _, part := range strings.Split(series[query], ";") {
			labels, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			result = append(result, fmt.Sprintf(`{"metric":%s,"values":[[%s,"%s"]]}`, labels, r.URL.Query().Get("end"), value))
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[`+strings.Join(result, ",")+`]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelsSectionQueries(t *testing.T) {
	v := reflect.ValueOf(adminModelQueries)
	for i := range v.NumField() {
		e := v.Field(i).String()
		assert.Equal(t, strings.Count(e, "$"), 1, "the window is the only value put in: "+e)
		assert.False(t, strings.Contains(e, "tenant_id="), "organisations are chosen through extra_filters only: "+e)
	}
}

func TestModelsSection(t *testing.T) {
	const gone = "0123456789abcdef01234567" // an organisation deleted since
	q := adminModelQueries
	l := func(tenant, provider, model string, extra ...string) string {
		s := `"tenant_id":"` + tenant + `","llm_provider":"` + provider + `","llm_model":"` + model + `"`
		for i := 0; i+1 < len(extra); i += 2 {
			s += `,"` + extra[i] + `":"` + extra[i+1] + `"`
		}
		return "{" + s + "}"
	}
	series := map[string]string{
		// admlocal/qwen: a local model; local calls without prices count
		// as pricing missing, but a local provider has no cost.
		// admpricey/m1: a configured cloud model without prices; every call
		// lacks one. admpricey/m2: a model of that provider the
		// configuration never named. admcloud/big: priced, one call
		// without reported usage.
		q.requests: l(gone, "admlocal", "qwen", "task", "review", "outcome", "ok") + "=9;" +
			l(gone, "admlocal", "qwen", "task", "review", "outcome", "error") + "=1;" +
			l(gone, "admpricey", "m1", "task", "briefing", "outcome", "ok") + "=4;" +
			l("", "admpricey", "m2", "task", "briefing", "outcome", "ok") + "=2;" +
			l("", "none", "none", "task", "other", "outcome", "ok") + "=1;" +
			l(gone, "admcloud", "big", "task", "review", "outcome", "ok") + "=3",
		q.input:          l(gone, "admlocal", "qwen", "task", "review") + "=5000;" + l(gone, "admpricey", "m1", "task", "briefing") + "=800;" + l(gone, "admcloud", "big", "task", "review") + "=2000",
		q.output:         l(gone, "admlocal", "qwen", "task", "review") + "=500;" + l(gone, "admcloud", "big", "task", "review") + "=300",
		q.cached:         l(gone, "admcloud", "big") + "=1000",
		q.cost:           l(gone, "admcloud", "big") + "=0.5;" + l(gone, "admlocal", "qwen") + "=0",
		q.cacheSaved:     l(gone, "admcloud", "big") + "=0.1",
		q.usageUnknown:   l(gone, "admcloud", "big") + "=1;" + l("", "none", "none") + "=1",
		q.pricingMissing: l(gone, "admlocal", "qwen") + "=10;" + l(gone, "admpricey", "m1") + "=4;" + l("", "admpricey", "m2") + "=2",
		q.cloudBytes:     l(gone, "admcloud", "big") + "=12000;" + l(gone, "admpricey", "m1") + "=3000",
		q.ttft:           `{"llm_provider":"admcloud","llm_model":"big"}=0.42;{"llm_provider":"admpricey","llm_model":"m2"}=9`,
		q.duration:       `{"llm_provider":"admcloud","llm_model":"big"}=1.5;{"llm_provider":"admlocal","llm_model":"qwen"}=NaN`,
		q.embedded:       `{}=7`,
		q.offload:        `{}=12`,
	}
	var filters []string
	vm := cannedVM(t, series, &filters)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "admlocal", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen"},
		{Name: "admpricey", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m1"},
		{Name: "admcloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", PriceInputPerMTok: 3, PriceOutputPerMTok: 15,
			PriceSource: "https://example.com/pricing", PriceChecked: "2026-10-20"},
	}, Default: "admlocal"})
	acc := accounts.NewService(acctmem.New(), false)
	s := &Server{cfg: Config{Accounts: acc, Metrics: &telemetry.Querier{URL: vm.URL}, LLM: reg}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d, err := s.modelStats(context.Background(), "7d", accounts.StatsQuery{Now: time.Now()})
	require.NoError(t, err)
	for _, f := range filters {
		assert.Equal(t, f, "", "the platform view reads every tenant")
	}
	rows := map[string]ModelRow{}
	for _, m := range d.models {
		rows[m.Provider+"/"+m.Model] = m
	}
	require.Len(t, d.models, 5)

	t.Run("cost is unknown when every call lacks a price", func(t *testing.T) {
		m := rows["admpricey/m1"]
		assert.Equal(t, m.Kind, modelCloud)
		assert.Equal(t, m.CostBasis, costUnknown)
		assert.True(t, m.CostDollars == nil && m.ProviderCacheSavedDollars == nil, "unknown, never $0")
		assert.Equal(t, m.PricingMissingCalls, 4.0)
		assert.False(t, m.Priced)
	})

	t.Run("a local provider has no cost", func(t *testing.T) {
		m := rows["admlocal/qwen"]
		assert.Equal(t, m.Kind, modelLocal)
		assert.Equal(t, m.CostBasis, costLocal)
		assert.True(t, m.CostDollars == nil, "no cost, not $0")
		assert.Equal(t, []float64{m.UnknownUsageCalls, m.PricingMissingCalls}, []float64{0, 0}, "local calls never count as unknown cost")
		assert.Equal(t, []float64{m.Requests, m.Failures, m.LocalRequests, m.CloudBytes}, []float64{10, 1, 10, 0})
		assert.True(t, m.RequestP95Seconds == nil, "a NaN quantile is left out")
	})

	t.Run("models outside the configuration are not named", func(t *testing.T) {
		_, named := rows["admpricey/m2"]
		assert.False(t, named)
		m := rows["admpricey/other"]
		assert.True(t, m.Folded)
		assert.Equal(t, []any{m.Kind, m.Requests, m.CostBasis}, []any{modelCloud, 2.0, costUnknown})
		assert.True(t, m.TTFTP95Seconds == nil, "a folded row has no quantile")
		u := rows["other/other"]
		assert.Equal(t, []any{u.Kind, u.Requests, u.UnknownUsageCalls, u.CostBasis}, []any{modelUnknown, 1.0, 1.0, costUnknown})
		body, err := json.Marshal([]any{d.totals, d.rows})
		require.NoError(t, err)
		out, err := json.Marshal(d.models)
		require.NoError(t, err)
		assert.False(t, strings.Contains(string(out)+string(body), `"m2"`), "m2 never appears")
		assert.Equal(t, d.models[len(d.models)-1].Folded, true, "folded rows come last")
	})

	t.Run("priced cloud model", func(t *testing.T) {
		m := rows["admcloud/big"]
		assert.Equal(t, m.CostBasis, costPartial, "one call reported no usage")
		require.True(t, m.CostDollars != nil && m.ProviderCacheSavedDollars != nil)
		assert.Equal(t, []float64{*m.CostDollars, *m.ProviderCacheSavedDollars}, []float64{0.5, 0.1})
		assert.Equal(t, []any{m.Priced, m.PriceSource, m.PriceChecked}, []any{true, "https://example.com/pricing", "2026-10-20"})
		require.True(t, m.TTFTP95Seconds != nil && m.RequestP95Seconds != nil)
		assert.Equal(t, []float64{*m.TTFTP95Seconds, *m.RequestP95Seconds}, []float64{0.42, 1.5})
		assert.Equal(t, m.CloudBytes, 12000.0)
		require.Len(t, m.Tasks, 1)
		assert.Equal(t, m.Tasks[0], ModelTask{Task: "review", Requests: 3, InputTokens: 2000, OutputTokens: 300})
	})

	t.Run("totals", func(t *testing.T) {
		tt := d.totals
		assert.Equal(t, []float64{tt.Requests, tt.Failures, tt.LocalRequests, tt.CloudRequests, tt.UnknownRequests}, []float64{20, 1, 10, 9, 1})
		assert.Equal(t, []float64{tt.UnknownUsageCalls, tt.PricingMissingCalls}, []float64{2, 6}, "billable calls only")
		assert.Equal(t, tt.CostBasis, costPartial)
		require.True(t, tt.CostDollars != nil && tt.LocalSharePct != nil)
		assert.Equal(t, *tt.CostDollars, 0.5)
		assert.Equal(t, *tt.LocalSharePct, 50.0)
		assert.Equal(t, []float64{tt.CloudBytes, tt.EmbeddedEntities, tt.OffloadTurns}, []float64{15000, 7, 12})
		assert.Equal(t, []float64{tt.LocalTokens, tt.CloudTokens}, []float64{5500, 3100})
	})

	t.Run("organisations", func(t *testing.T) {
		require.Len(t, d.rows, 1, "calls without a tenant count in the totals only")
		o := d.rows[0]
		assert.Equal(t, []any{o.OrgID, o.Name, o.Deleted, o.Requests}, []any{gone, deletedOrg, true, 17.0})
	})

	t.Run("one organisation", func(t *testing.T) {
		filters = nil
		_, err := s.modelStats(context.Background(), "24h", accounts.StatsQuery{Now: time.Now(), OrgID: gone})
		require.NoError(t, err)
		require.True(t, len(filters) > 0)
		for _, f := range filters {
			assert.Contains(t, f, gone, "every query is scoped through extra_filters")
		}
	})

	t.Run("every call lacks a price", func(t *testing.T) {
		f := sumModelCells([]*modelCell{{requests: 3, pricingMissing: 2, usageUnknown: 1, kind: modelCloud}})
		assert.Equal(t, f.CostBasis, costUnknown)
		assert.True(t, f.CostDollars == nil)
		f = sumModelCells([]*modelCell{{requests: 3, kind: modelLocal, pricingMissing: 3}})
		assert.Equal(t, f.CostBasis, costLocal)
		assert.True(t, f.CostDollars == nil)
		f = sumModelCells(nil)
		assert.Equal(t, f.CostBasis, costNone)
		assert.True(t, f.CostDollars == nil && f.LocalSharePct == nil)
	})

	t.Run("unknown ranges and missing metrics", func(t *testing.T) {
		_, err := s.modelStats(context.Background(), "2y", accounts.StatsQuery{Now: time.Now()})
		assert.ErrorIs(t, err, errModelsRange)
		assert.False(t, modelsRange("2y"))
		assert.True(t, modelsRange("90d") && modelsRange("365d") && modelsRange("1h"))
		off := &Server{cfg: Config{Accounts: acc}}
		_, err = off.modelStats(context.Background(), "7d", accounts.StatsQuery{Now: time.Now()})
		assert.ErrorIs(t, err, errNoMetrics)
	})
}

func TestModelAndStoreRollupSources(t *testing.T) {
	const org = "0123456789abcdef01234567"
	day := func(expr string) string { return strings.ReplaceAll(expr, "$r", "1d") }
	q := adminModelQueries
	l := func(tenant, provider string, extra ...string) string {
		s := `"tenant_id":"` + tenant + `","llm_provider":"` + provider + `","llm_model":"m"`
		for i := 0; i+1 < len(extra); i += 2 {
			s += `,"` + extra[i] + `":"` + extra[i+1] + `"`
		}
		return "{" + s + "}"
	}
	series := map[string]string{
		day(q.requests): l(org, "rolllocal", "task", "review", "outcome", "ok") + "=4;" +
			l(org, "rollcloud", "task", "review", "outcome", "error") + "=2;" +
			l(telemetry.NoTenant, "rollcloud", "task", "other", "outcome", "ok") + "=1",
		day(q.input):    l(org, "rollcloud", "task", "review") + "=100",
		day(q.cost):     l(org, "rollcloud") + "=0.25",
		day(q.embedded): `{"tenant_id":"` + org + `"}=3`,
		storeRollupQueries[accounts.StatStoreRows]: `{"db_system":"postgres","kind":"database"}=42;` +
			`{"db_system":"postgres","kind":"not_registered"}=9;{"db_system":"postgres","kind":"database"}=NaN`,
		storeRollupQueries[accounts.StatStoreBytes]: `{"db_system":"postgres","kind":"database"}=4096`,
	}
	var filters []string
	vm := cannedVM(t, series, &filters)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "rolllocal", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "m"},
		{Name: "rollcloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m"},
	}, Default: "rolllocal"})
	s := &Server{cfg: Config{Accounts: accounts.NewService(acctmem.New(), false), Metrics: &telemetry.Querier{URL: vm.URL}, LLM: reg},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d := accounts.StatDay(time.Now()).AddDate(0, 0, -1)
	set := accounts.NewPlatformStatSet(d)
	require.NoError(t, s.llmStatSource(context.Background(), d, set))
	require.NoError(t, s.storeStatSource(context.Background(), d, set))
	got := map[string]int64{}
	for _, r := range set.Rows() {
		require.True(t, accounts.ValidPlatformStat(r.Metric, r.Dimension, r.OrgID), "every row passes the allowlist: "+r.Metric+"/"+r.Dimension)
		if r.Value != 0 {
			got[r.OrgID+"|"+r.Metric+"/"+r.Dimension] = r.Value
		}
	}
	assert.Equal(t, got, map[string]int64{
		"|llm_calls/local.requests": 4, org + "|llm_calls/local.requests": 4,
		"|llm_calls/cloud.requests": 3, org + "|llm_calls/cloud.requests": 2,
		"|llm_calls/cloud.failures": 2, org + "|llm_calls/cloud.failures": 2,
		"|llm_calls/cloud.input": 100, org + "|llm_calls/cloud.input": 100,
		"|llm_calls/cloud.cost_micro": 250000, org + "|llm_calls/cloud.cost_micro": 250000,
		"|llm_calls/measured":  1,
		"|llm_memory/embedded": 3, org + "|llm_memory/embedded": 3,
		"|store_rows/postgres.database": 42, "|store_bytes/postgres.database": 4096,
	})
	for _, f := range filters {
		assert.Equal(t, f, "", "the rollup reads every tenant")
	}

	t.Run("without a metrics store nothing is measured", func(t *testing.T) {
		off := &Server{cfg: Config{Accounts: s.cfg.Accounts}, logger: s.logger}
		set := accounts.NewPlatformStatSet(d)
		require.NoError(t, off.llmStatSource(context.Background(), d, set))
		require.NoError(t, off.storeStatSource(context.Background(), d, set))
		for _, r := range set.Rows() {
			assert.Equal(t, r.Value, int64(0), r.Metric+"/"+r.Dimension)
			assert.False(t, r.Metric == accounts.StatStoreRows, "a day without a sample writes no store rows")
		}
	})
}
