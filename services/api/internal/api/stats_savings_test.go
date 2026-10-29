package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// A Thursday afternoon, so the weekly layout starts and ends mid-week.
var savingsNow = time.Date(2026, 10, 29, 15, 4, 5, 0, time.UTC)

func TestSavingsLayout(t *testing.T) {
	today := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		days    int
		step    string
		buckets int
		first   time.Time
		second  time.Time
	}{
		{1, savingsHour, 24, today, today.Add(time.Hour)},
		{2, savingsSix, 8, today.AddDate(0, 0, -1), today.AddDate(0, 0, -1).Add(6 * time.Hour)},
		{7, savingsSix, 28, today.AddDate(0, 0, -6), today.AddDate(0, 0, -6).Add(6 * time.Hour)},
		{8, savingsDay, 8, today.AddDate(0, 0, -7), today.AddDate(0, 0, -6)},
		{30, savingsDay, 30, today.AddDate(0, 0, -29), today.AddDate(0, 0, -28)},
		// 90 days from Saturday 1 August: a partial first week to Monday 3
		// August, 12 whole weeks, and Monday 26 to Thursday 29 October.
		{90, savingsWeek, 14, today.AddDate(0, 0, -89), time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(strconv.Itoa(tc.days), func(t *testing.T) {
			l := layoutFor(tc.days, savingsNow)
			assert.Equal(t, l.step, tc.step)
			require.Len(t, l.bounds, tc.buckets+1)
			assert.Equal(t, l.bounds[0], tc.first)
			assert.Equal(t, l.bounds[1], tc.second)
			assert.Equal(t, l.bounds[len(l.bounds)-1], today.AddDate(0, 0, 1), "the window ends at the coming midnight UTC")
			for i := 1; i < len(l.bounds); i++ {
				assert.True(t, l.bounds[i].After(l.bounds[i-1]), "bounds rise")
				if tc.step == savingsWeek && i < len(l.bounds)-1 {
					assert.Equal(t, l.bounds[i].Weekday(), time.Monday, "weeks start on Monday")
				}
			}
		})
	}
	// Midnight belongs to the day it starts.
	l := layoutFor(1, today)
	assert.Equal(t, l.bounds[0], today)
}

func TestApportion(t *testing.T) {
	for name, tc := range map[string]struct {
		total   int
		weights []float64
		want    []int
	}{
		"even":            {9, []float64{1, 1, 1}, []int{3, 3, 3}},
		"remainder":       {10, []float64{1, 1, 1}, []int{4, 3, 3}},
		"largest rest":    {10, []float64{1, 2, 4}, []int{1, 3, 6}},
		"empty slots":     {7, []float64{0, 5, 0, 0}, []int{0, 7, 0, 0}},
		"nothing to give": {0, []float64{0, 0}, []int{0, 0}},
		"no shape":        {7, []float64{0, 0}, nil},
		"no slots":        {7, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := apportion(tc.total, tc.weights)
			assert.Equal(t, got, tc.want)
			if got != nil {
				n := 0
				for _, x := range got {
					n += x
				}
				assert.Equal(t, n, tc.total, "the parts add up to the day")
			}
		})
	}
}

func TestBucketOf(t *testing.T) {
	l := layoutFor(90, savingsNow)
	assert.Equal(t, bucketOf(l.bounds, l.bounds[0]), 0)
	assert.Equal(t, bucketOf(l.bounds, l.bounds[1].Add(-time.Second)), 0)
	assert.Equal(t, bucketOf(l.bounds, l.bounds[1]), 1)
	assert.Equal(t, bucketOf(l.bounds, l.bounds[len(l.bounds)-1]), len(l.bounds)-2, "the end belongs to the last bucket")
}

// savingsUsage is 90 days of calls: a client that reports repeats (with
// repeats, re-sends and hook tokens), one that does not, and ask_local.
func savingsUsage() []usage.Bucket {
	var out []usage.Bucket
	for i := range 90 {
		if i%5 == 3 {
			continue // days without calls
		}
		day := usage.Day(savingsNow.AddDate(0, 0, -i))
		out = append(out,
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: "search", Client: "claude-code", Calls: 3, ResponseTokens: 1000 + 37*i, RangesReturned: 4},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: "get_symbol_context", Client: "claude-code", Calls: 2, Errors: i % 2, ResponseTokens: 777 + i},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: usage.RepeatAware, Client: "claude-code", Calls: 1},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: usage.RepeatTool("search"), Client: "claude-code", Calls: 1 + i%3},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: usage.ResendTool("search"), Client: "claude-code", Calls: 5},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: "file_outline", Client: "codex", Calls: 1, ResponseTokens: 333 + 11*i},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: "ask_local", Client: "claude-code", Calls: 1, ResponseTokens: 200},
			usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: usage.LocalAvoided, Client: "claude-code", Calls: 900 + i},
		)
		tc := usage.TokenCounts{Input: 50_000, Output: 6000, CachedInput: 20_000, CacheWrite: 5000, Turns: 4, CacheKnownTurns: 4}
		for tool, n := range tc.Counters() {
			if n > 0 {
				out = append(out, usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: tool, Client: "claude-code", Calls: n})
			}
		}
	}
	return out
}

// The chart's graph answers add up to the tiles of the same window: tokens
// exactly, dollars to the cent.
func TestSavingsSumsEqualTiles(t *testing.T) {
	buckets := savingsUsage()
	repos := []RepoSummary{{Name: "r", GraphTokens: 1000, BaseTokens: 4000}}
	for _, reg := range []*llm.Registry{nil, llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", PriceInputPerMTok: 3, PriceOutputPerMTok: 15, PriceCachedInputPerMTok: 0.3},
	}, Default: "cloud"})} {
		s := &Server{cfg: Config{LLM: reg}}
		for _, days := range []int{1, 2, 7, 30, 31, 90, 180} {
			t.Run(fmt.Sprintf("%d days, priced %v", days, reg != nil), func(t *testing.T) {
				tile := agentUsage(buckets, repos, days, savingsNow)
				saved := savedEstimate(tile, s.priceReference())
				require.True(t, saved.TotalDollars != nil)
				want := tile.AvoidedTokens - tile.LocalAvoidedTokens
				require.True(t, want > 0)

				l := layoutFor(days, savingsNow)
				var slots []savingsSlot
				if l.slot < 24*time.Hour {
					// A measured shape for every day but the first, which is left
					// unplaced.
					slots = make([]savingsSlot, len(l.bounds)-1)
					perDay := int(24 * time.Hour / l.slot)
					for k := range slots {
						if k >= perDay || days == 1 {
							slots[k].graphShape = float64(k%3) * 10
						}
					}
				}
				got := s.buildSavings(l, buckets, repos, slots)
				require.Len(t, got.Buckets, len(l.bounds)-1)
				tokens, dollars := got.UnplacedTokens, got.UnplacedDollars
				for _, b := range got.Buckets {
					tokens += b.NeverReadTokens
					if b.GraphSaved != nil {
						dollars += *b.GraphSaved
					}
				}
				assert.Equal(t, tokens, want)
				assert.True(t, math.Abs(dollars-*saved.TotalDollars) < 0.01, fmt.Sprintf("buckets %.4f, tile %.4f", dollars, *saved.TotalDollars))
				if l.slot < 24*time.Hour && days > 1 {
					assert.True(t, got.UnplacedTokens > 0, "the first day has no shape")
				}
			})
		}
	}
}

// Days without calls are empty buckets, not missing ones.
func TestSavingsEmptyBuckets(t *testing.T) {
	s := &Server{}
	got := s.buildSavings(layoutFor(30, savingsNow), nil, nil, nil)
	require.Len(t, got.Buckets, 30)
	assert.Equal(t, got.Step, savingsDay)
	assert.Equal(t, got.Days, 30)
	assert.False(t, got.Metrics)
	for _, b := range got.Buckets {
		assert.Equal(t, b.NeverReadTokens, 0)
		assert.True(t, b.GraphSaved != nil && *b.GraphSaved == 0, "known, and zero")
		assert.True(t, b.CompressionSaved == nil && b.GatewaySaved == nil, "unknown without the metrics store")
		assert.Equal(t, b.End-b.Start, int64(86400))
	}
}

// savingsVM answers each savings query with one point per slot, valued by
// the query and the slot's end, and records the extra filters it was sent.
func savingsVM(t *testing.T, filters *[]string) *httptest.Server {
	t.Helper()
	q := savingsQueries
	series := map[string]string{
		q.input:      `{"llm_provider":"local"}=1000;{"llm_provider":"cloud"}=50`,
		q.output:     `{"llm_provider":"local"}=100`,
		q.cached:     `{"llm_provider":"local"}=10`,
		q.compressed: `{"llm_provider":"cloud"}=4000`,
		q.gateway:    `{"kind":"input"}=10000;{"kind":"output"}=1000`,
		q.unmetered:  ``,
		q.graphShape: `{}=5`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*filters = append(*filters, strings.Join(r.URL.Query()["extra_filters[]"], ";"))
		query := r.URL.Query().Get("query")
		step, _ := strconv.Atoi(strings.TrimSuffix(r.URL.Query().Get("step"), "s")) //nolint:errcheck // test server
		query = strings.ReplaceAll(query, "["+strconv.Itoa(step)+"s]", "[$w]")
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64) //nolint:errcheck // test server
		end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)     //nolint:errcheck // test server
		var result []string
		for _, part := range strings.Split(series[query], ";") {
			labels, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			var points []string
			for ts := start; ts <= end; ts += int64(step) {
				points = append(points, fmt.Sprintf(`[%d,"%s"]`, ts, value))
			}
			result = append(result, fmt.Sprintf(`{"metric":%s,"values":[%s]}`, labels, strings.Join(points, ",")))
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[`+strings.Join(result, ",")+`]}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Local model work, compression and the gateway come from the metrics store
// slot by slot, valued with the tiles' own functions, and weeks add up their
// days.
func TestSavingsMetrics(t *testing.T) {
	var filters []string
	vm := savingsVM(t, &filters)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen"},
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", PriceInputPerMTok: 3, PriceOutputPerMTok: 15},
	}, Default: "local"})
	s := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: vm.URL}, LLM: reg}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	tenant := []string{"0123456789abcdef01234567"}
	ref := s.priceReference()
	for _, tc := range []struct {
		days, buckets int
		slotsIn       []int // slots per bucket, first and last
	}{
		{1, 24, []int{1, 1}},
		{7, 28, []int{1, 1}},
		{30, 30, []int{1, 1}},
		{90, 14, []int{2, 4}},
	} {
		t.Run(strconv.Itoa(tc.days), func(t *testing.T) {
			filters = filters[:0]
			l := layoutFor(tc.days, savingsNow)
			slots, err := s.metricsSlots(context.Background(), l, tenant)
			require.NoError(t, err)
			require.Len(t, slots, int(l.bounds[len(l.bounds)-1].Sub(l.bounds[0])/l.slot))
			got := s.buildSavings(l, nil, nil, slots)
			require.Len(t, got.Buckets, tc.buckets)
			assert.True(t, got.Metrics)
			for i, n := range map[int]int{0: tc.slotsIn[0], tc.buckets - 1: tc.slotsIn[1]} {
				b := got.Buckets[i]
				assert.Equal(t, []float64{b.LocalInputTokens, b.LocalOutputTokens, b.LocalCachedTokens}, []float64{float64(1000 * n), float64(100 * n), float64(10 * n)})
				require.True(t, b.CompressionSaved != nil && b.GatewaySaved != nil)
				assert.Equal(t, *b.CompressionSaved, round4(float64(n)*1000*3/1e6), "4000 bytes are 1000 tokens at $3 a million")
				assert.Equal(t, *b.GatewaySaved, round4(float64(n)*gatewaySaved(GatewayStat{LocalInputTokens: 10000, LocalOutputTokens: 1000}, ref)))
			}
			assert.Equal(t, len(filters), 7)
			for _, f := range filters {
				assert.Contains(t, f, tenant[0], "every query is scoped to the caller's organisations")
			}
		})
	}
}

// An unmetered local answer makes its bucket's gateway saving unknown, like
// the tile's.
func TestSavingsUnmeteredGateway(t *testing.T) {
	s := &Server{}
	l := layoutFor(90, savingsNow)
	slots := make([]savingsSlot, int(l.bounds[len(l.bounds)-1].Sub(l.bounds[0])/l.slot))
	for k := range slots {
		slots[k].model = map[string]*ModelStat{}
		slots[k].gw.LocalInputTokens = 1000
	}
	slots[1].gw.LocalUnmetered = 1
	got := s.buildSavings(l, nil, nil, slots)
	assert.True(t, got.Buckets[0].GatewaySaved == nil, "one unmetered day in the week")
	assert.True(t, got.Buckets[1].GatewaySaved != nil)
}

func TestModelWindowDays(t *testing.T) {
	end := savingsNow
	for _, tc := range []struct {
		query, name, window string
		span                time.Duration
		bad                 bool
	}{
		{"range=24h", "24h", "24h", 24 * time.Hour, false},
		{"", "7d", "7d", 7 * 24 * time.Hour, false},
		{"days=1", "1d", "54245s", 15*time.Hour + 4*time.Minute + 5*time.Second, false},
		{"days=2", "2d", "140645s", 39*time.Hour + 4*time.Minute + 5*time.Second, false},
		{"days=0", "", "", 0, true},
		{"days=181", "", "", 0, true},
		{"range=1y", "", "", 0, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			require.NoError(t, err)
			name, window, span, err := modelWindow(q, end)
			if tc.bad {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []any{name, window, span}, []any{tc.name, tc.window, tc.span})
		})
	}
}
