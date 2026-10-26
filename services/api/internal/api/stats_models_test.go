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
		q.requests: `{"llm_provider":"local","llm_model":"qwen","task":"review","outcome":"ok"}=8;{"llm_provider":"local","llm_model":"qwen","task":"review","outcome":"error"}=2;{"llm_provider":"local","llm_model":"qwen","task":"local-answer","outcome":"ok"}=30;{"llm_provider":"cloud","llm_model":"big","task":"briefing","outcome":"ok"}=1`,
		q.input:    `{"llm_provider":"local","llm_model":"qwen","task":"review"}=40000;{"llm_provider":"local","llm_model":"qwen","task":"local-answer"}=200000;{"llm_provider":"cloud","llm_model":"big","task":"briefing"}=900`,
		q.output:   `{"llm_provider":"local","llm_model":"qwen","task":"review"}=3000;{"llm_provider":"local","llm_model":"qwen","task":"local-answer"}=9000`,
		q.readS:    `{"llm_provider":"local","llm_model":"qwen"}=500`,
		q.writeS:   `{"llm_provider":"local","llm_model":"qwen"}=400`,
		q.readT:    `{"llm_provider":"local","llm_model":"qwen"}=220000`,
		q.writeT:   `{"llm_provider":"local","llm_model":"qwen"}=12000`,
		q.p50:      `{"llm_provider":"local","llm_model":"qwen"}=0.0213`,
		q.p95:      `{"llm_provider":"local","llm_model":"qwen"}=NaN`,
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
	assert.True(t, c.ReadTPS == nil, "no streamed answers, no speed")
	for _, f := range filters {
		assert.Contains(t, f, "0123456789abcdef01234567", "every query is scoped to the caller's organisations")
	}

	w = httptest.NewRecorder()
	s.handleModelStats(w, httptest.NewRequest(http.MethodGet, "/api/v1/stats/models?range=1y", nil).WithContext(ctx))
	assert.Equal(t, w.Code, http.StatusBadRequest)
}

// ask_local's counters are tokens, never tool calls.
func TestAgentStatsCountsAskLocal(t *testing.T) {
	now := time.Now()
	day := usage.Day(now)
	a := agentStats([]usage.Bucket{
		{Day: day, Repo: "r", Tool: "ask_local", Client: "claude-code", Calls: 1, ResponseTokens: 300},
		{Day: day, Repo: "r", Tool: usage.LocalRead, Client: "claude-code", Calls: 11000},
		{Day: day, Repo: "r", Tool: usage.LocalAvoided, Client: "claude-code", Calls: 9700},
	}, 7, now, nil, 1)
	assert.Equal(t, a.Calls, 1)
	assert.Equal(t, a.LocalReadTokens, 11000)
	assert.Equal(t, a.AvoidedTokens, 9700)
	assert.Equal(t, a.LocalAvoidedTokens, 9700)
	assert.Equal(t, a.Daily[len(a.Daily)-1].AvoidedTokens, 9700)
	assert.Equal(t, a.ContextTotalTokens, 10000)
}
