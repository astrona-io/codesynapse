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
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// modelVM answers each fixed model query with canned series.
func modelVM(t *testing.T, filters *[]string) *httptest.Server {
	t.Helper()
	q := modelQueries
	series := map[string]string{
		q.requests:                  `{"llm_provider":"local","llm_model":"qwen","task":"review","outcome":"ok"}=8;{"llm_provider":"local","llm_model":"qwen","task":"review","outcome":"error"}=1;{"llm_provider":"local","llm_model":"qwen","task":"review","outcome":"timeout"}=1;{"llm_provider":"local","llm_model":"qwen","task":"local-answer","outcome":"ok"}=30;{"llm_provider":"cloud","llm_model":"big","task":"briefing","outcome":"ok"}=1`,
		q.input:                     `{"llm_provider":"local","llm_model":"qwen","task":"review"}=40000;{"llm_provider":"local","llm_model":"qwen","task":"local-answer"}=200000;{"llm_provider":"cloud","llm_model":"big","task":"briefing"}=900`,
		q.output:                    `{"llm_provider":"local","llm_model":"qwen","task":"review"}=3000;{"llm_provider":"local","llm_model":"qwen","task":"local-answer"}=9000`,
		q.readS:                     `{"llm_provider":"local","llm_model":"qwen"}=500`,
		q.writeS:                    `{"llm_provider":"local","llm_model":"qwen"}=400`,
		q.readT:                     `{"llm_provider":"local","llm_model":"qwen"}=220000`,
		q.writeT:                    `{"llm_provider":"local","llm_model":"qwen"}=12000`,
		q.p50:                       `{"llm_provider":"local","llm_model":"qwen"}=0.0213`,
		q.p95:                       `{"llm_provider":"local","llm_model":"qwen"}=NaN`,
		q.cost:                      `{"llm_provider":"cloud","llm_model":"big"}=0.0123`,
		q.compressed:                `{"llm_provider":"cloud","llm_model":"big"}=40000`,
		gatewayQueries.requests:     `{"route":"local","class":"2xx"}=6;{"route":"cloud","class":"2xx"}=3;{"route":"cloud","class":"5xx"}=1;{"route":"rejected","class":"4xx"}=2`,
		gatewayQueries.bytes:        `{"route":"local","direction":"in"}=1000;{"route":"cloud","direction":"in"}=3000;{"route":"local","direction":"out"}=500`,
		gatewayQueries.tokens:       `{"kind":"input"}=100000;{"kind":"output"}=20000`,
		gatewayQueries.unmetered:    ``,
		gatewayQueries.fallthroughs: `{"reason":"queue_timeout"}=2;{"reason":"answer_timeout"}=1;{"reason":"has_image"}=1;{"reason":"made_up"}=1`,
		gatewayQueries.latencySum:   `{"route":"local"}=12;{"route":"cloud"}=20`,
		gatewayQueries.latencyCount: `{"route":"local"}=6;{"route":"cloud"}=4`,
		gatewayQueries.latencyP95:   `{"route":"local"}=4.5;{"route":"cloud"}=NaN`,
		gatewayQueries.queueSum:     `{}=30`,
		gatewayQueries.queueCount:   `{}=10`,
		gatewayQueries.queueP95:     `{}=19.5`,
		gatewayQueries.answerSum:    `{}=9`,
		gatewayQueries.answerN:      `{}=6`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*filters = append(*filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
		query := strings.ReplaceAll(r.URL.Query().Get("query"), "[7d]", "[$r]")
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

func TestModelStats(t *testing.T) {
	var filters []string
	vm := modelVM(t, &filters)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen", ContextWindow: 262144},
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", PriceInputPerMTok: 3, PriceOutputPerMTok: 15},
	}, Default: "local"})
	s := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: vm.URL}, LLM: reg}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.WithValue(context.Background(), scopeKey{}, scope{orgs: []accounts.Membership{{Org: accounts.Org{ID: "0123456789abcdef01234567"}}}})
	w := httptest.NewRecorder()
	s.handleModelStats(w, httptest.NewRequest(http.MethodGet, "/api/v1/stats/models?range=7d", nil).WithContext(ctx))
	require.Equal(t, w.Code, http.StatusOK, w.Body.String())
	var out ModelStats
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Models, 2)
	require.True(t, out.Reference != nil)
	assert.Equal(t, *out.Reference, PriceReference{Provider: "cloud", Model: "big", InputPerMTok: 3, OutputPerMTok: 15})
	q := out.Models[0]
	assert.Equal(t, []any{q.Provider, q.Model, q.Local, q.Configured, q.ContextWindow}, []any{"local", "qwen", true, true, 262144})
	assert.Equal(t, []float64{q.Requests, q.Failures, q.InputTokens, q.OutputTokens, q.AvgPromptTokens}, []float64{40, 2, 240000, 12000, 6000})
	require.True(t, q.ReadTPS != nil && q.WriteTPS != nil)
	assert.Equal(t, []float64{*q.ReadTPS, *q.WriteTPS}, []float64{440, 30})
	require.True(t, q.ContextP50 != nil)
	assert.Equal(t, *q.ContextP50, 0.021)
	assert.True(t, q.ContextP95 == nil, "NaN quantiles are left out")
	require.Len(t, q.Tasks, 2)
	assert.Equal(t, q.Tasks[0], ModelTask{Task: "local-answer", Requests: 30, InputTokens: 200000, OutputTokens: 9000})
	assert.Equal(t, q.Tasks[1].Failures, 2.0)
	c := out.Models[1]
	assert.False(t, c.Local)
	require.True(t, out.Spent != nil && out.CompressionSaved != nil)
	assert.Equal(t, *out.Spent, 0.0123)
	assert.Equal(t, *out.CompressionSaved, 0.03, "10000 tokens at $3 per million")
	assert.Equal(t, c.CompressedBytes, 40000.0)
	assert.True(t, out.CompressionEnabled, "every task routes locally now, but the window holds shortened prompts")
	assert.Equal(t, out.CompressionOff, "")
	assert.True(t, c.ReadTPS == nil, "no streamed answers, no speed")
	gw := out.Gateway
	assert.Equal(t, []float64{gw.Requests, gw.Local, gw.Cloud, gw.Rejected, gw.Failed, gw.LocalSharePct}, []float64{12, 6, 4, 2, 3, 50})
	assert.Equal(t, []float64{gw.BytesIn, gw.BytesOut}, []float64{4000, 500})
	require.True(t, gw.SavedDollars != nil)
	assert.Equal(t, *gw.SavedDollars, 0.6, "100k in at $3 and 20k out at $15 per million")
	assert.Equal(t, []float64{gw.Fallthroughs, gw.Timeouts, gw.TimeoutSharePct}, []float64{5, 3, 25})
	assert.Equal(t, gw.FallthroughReasons, map[string]float64{"queue_timeout": 2, "answer_timeout": 1, "has_image": 1, "other": 1}, "reasons outside the fixed set are other")
	require.True(t, gw.LocalAvgSeconds != nil && gw.CloudAvgSeconds != nil && gw.LocalP95Seconds != nil)
	assert.Equal(t, []float64{*gw.LocalAvgSeconds, *gw.CloudAvgSeconds, *gw.LocalP95Seconds}, []float64{2, 5, 4.5})
	assert.True(t, gw.CloudP95Seconds == nil, "a NaN quantile is left out")
	require.True(t, gw.QueueAvgSeconds != nil && gw.QueueP95Seconds != nil && gw.LocalAnswerAvgSeconds != nil)
	assert.Equal(t, []float64{*gw.QueueAvgSeconds, *gw.QueueP95Seconds, *gw.LocalAnswerAvgSeconds}, []float64{3, 19.5, 1.5})
	for _, f := range filters {
		assert.Contains(t, f, "0123456789abcdef01234567", "every query is scoped to the caller's organisations")
	}

	w = httptest.NewRecorder()
	s.handleModelStats(w, httptest.NewRequest(http.MethodGet, "/api/v1/stats/models?range=1y", nil).WithContext(ctx))
	assert.Equal(t, w.Code, http.StatusBadRequest)
}

// A cloud model that worked without a price makes spend and compression
// savings unknown, not zero.
func TestMoneyTotalsUnknownWithoutPrices(t *testing.T) {
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big"},
	}, Default: "cloud"})
	s := &Server{cfg: Config{LLM: reg}}
	spent, comp := s.moneyTotals([]ModelStat{{Provider: "cloud", Model: "big", InputTokens: 900, CompressedBytes: 400}})
	assert.True(t, spent == nil)
	assert.True(t, comp == nil)
	spent, comp = s.moneyTotals([]ModelStat{{Provider: "local", Model: "qwen", Local: true, InputTokens: 5}})
	assert.True(t, spent != nil && *spent == 0)
	assert.True(t, comp != nil && *comp == 0)
}

// The tile says why prompts are not shortened instead of showing $0.
func TestCompressionState(t *testing.T) {
	cloud := llm.ProviderConfig{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big"}
	local := llm.ProviderConfig{Name: "local", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen"}
	shortened := []ModelStat{{Provider: "cloud", Model: "big", CompressedBytes: 400}}
	for name, tc := range map[string]struct {
		reg    *llm.Registry
		models []ModelStat
		on     bool
		reason string
	}{
		"no AI":                            {nil, nil, false, "no_ai"},
		"cloud only":                       {llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cloud}, Default: "cloud"}), nil, false, llm.CompressionNoLocalModel},
		"turned off":                       {llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cloud, local}, Default: "cloud", CompressPrompts: new(false)}), shortened, false, llm.CompressionDisabled},
		"all local":                        {llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cloud, local}, Default: "local"}), nil, false, llm.CompressionNoCloudTask},
		"all local now, shortened earlier": {llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cloud, local}, Default: "local"}), shortened, true, ""},
		"local model on":                   {llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cloud, local}, Default: "cloud"}), nil, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			on, reason := compressionState(tc.reg, tc.models)
			assert.Equal(t, on, tc.on)
			assert.Equal(t, reason, tc.reason)
		})
	}
}

// ask_local's counters are tokens, never tool calls.
func TestAgentStatsCountsAskLocal(t *testing.T) {
	now := time.Now()
	day := usage.Day(now)
	a := agentStats([]usage.Bucket{
		{Day: day, Repo: "r", Tool: "ask_local", Client: "claude-code", Calls: 1, ResponseTokens: 300},
		{Day: day, Repo: "r", Tool: usage.LocalRead, Client: "claude-code", Calls: 11000},
		{Day: day, Repo: "r", Tool: usage.LocalAvoided, Client: "claude-code", Calls: 9700},
		{Day: day, Repo: "r", Tool: "local_draft", Client: "claude-code", Calls: 1, ResponseTokens: 200},
		{Day: day, Repo: "r", Tool: usage.LocalDrafts, Client: "claude-code", Calls: 1},
	}, 7, now, nil, 1)
	assert.Equal(t, a.Calls, 2)
	assert.Equal(t, a.LocalDrafts, 1, "a draft is counted, and adds nothing to tokens avoided")
	assert.Equal(t, a.LocalReadTokens, 11000)
	assert.Equal(t, a.AvoidedTokens, 9700)
	assert.Equal(t, a.LocalAvoidedTokens, 9700)
	assert.Equal(t, a.Daily[len(a.Daily)-1].AvoidedTokens, 9700)
	assert.Equal(t, a.ContextTotalTokens, 10200, "the draft the agent got joins its context")
}

func TestGatewaySaved(t *testing.T) {
	ref := PriceReference{InputPerMTok: 4, OutputPerMTok: 20, CachedInputPerMTok: 0.2}
	for _, tc := range []struct {
		name string
		gw   GatewayStat
		ref  PriceReference
		want float64
	}{
		{"no cache reported", GatewayStat{LocalInputTokens: 1_000_000, LocalOutputTokens: 100_000}, ref, 6},
		{"cached input at the cache-read price", GatewayStat{LocalInputTokens: 1_000_000, LocalOutputTokens: 100_000, LocalCachedTokens: 500_000}, ref, 4.1},
		{"no cache-read price: input price", GatewayStat{LocalInputTokens: 1_000_000, LocalCachedTokens: 500_000}, PriceReference{InputPerMTok: 4}, 4},
		{"cached never above input", GatewayStat{LocalInputTokens: 100_000, LocalCachedTokens: 900_000}, ref, 0.02},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, gatewaySaved(tc.gw, tc.ref), tc.want)
		})
	}
}
