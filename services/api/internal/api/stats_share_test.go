package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// shareFixture is one day of one client: a search call whose 1,000 response
// tokens avoid 3,000 (workspace ratio 4), and the hook's token counters.
type shareFixture struct {
	aware                bool
	returned, repeats    int
	resend               int
	fresh, cached, write int
	output, turns, known int
	extra                []usage.Bucket
	noTokens             bool
}

func (f shareFixture) stats(t *testing.T) AgentStats {
	t.Helper()
	now := time.Now()
	day := usage.Day(now)
	b := func(tool string, calls int) usage.Bucket {
		return usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: tool, Client: "claude-code", Calls: calls}
	}
	buckets := []usage.Bucket{{Day: day, Org: "o", Tool: "search", Client: "claude-code", Calls: 1, ResponseTokens: 1000}}
	if f.returned > 0 {
		buckets = append(buckets, usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: "search", Client: "claude-code", RangesReturned: f.returned})
	}
	if f.aware {
		buckets = append(buckets, b(usage.RepeatAware, 1))
	}
	if f.repeats > 0 {
		buckets = append(buckets, b(usage.RepeatTool("search"), f.repeats))
	}
	if f.resend > 0 {
		buckets = append(buckets, b(usage.ResendTool("search"), f.resend))
	}
	if !f.noTokens {
		tc := usage.TokenCounts{Input: f.fresh + f.cached + f.write, Output: f.output, CachedInput: f.cached, CacheWrite: f.write, Turns: f.turns, CacheKnownTurns: f.known}
		require.NoError(t, tc.Validate())
		for tool, n := range tc.Counters() {
			if n > 0 {
				buckets = append(buckets, b(tool, n))
			}
		}
	}
	buckets = append(buckets, f.extra...)
	return agentStats(buckets, 7, now, nil, 4)
}

func known(f shareFixture) shareFixture {
	f.aware, f.fresh, f.write, f.output, f.turns, f.known = true, 40_000, 10_000, 7_000, 10, 10
	return f
}

func TestShareIgnoresCacheReads(t *testing.T) {
	none := known(shareFixture{}).stats(t)
	require.True(t, none.ShareAvoidedKnown)
	require.NotNil(t, none.ShareAvoidedPct)
	// W = 40,000 fresh + 10,000 written + 7,000 output; A = 3,000.
	assert.Equal(t, none.WorkTokens, 57_000)
	assert.Equal(t, *none.ShareAvoidedPct, round1(100*3000.0/60_000))
	for _, cached := range []int{950_000, 1_900_000} {
		f := known(shareFixture{})
		f.cached, f.fresh = cached, 40_000
		got := f.stats(t)
		require.NotNil(t, got.ShareAvoidedPct)
		assert.Equal(t, *got.ShareAvoidedPct, *none.ShareAvoidedPct, "cache reads change nothing")
		assert.Equal(t, got.WorkTokens, none.WorkTokens)
		assert.True(t, *got.ShareRawPct < *none.ShareRawPct, "only the raw figure has cache reads in it")
	}
	// The plan's fixture: 950,000 cache reads in 1,000,000 input.
	f := known(shareFixture{})
	f.fresh, f.write, f.cached = 50_000, 0, 950_000
	g := known(shareFixture{})
	g.fresh, g.write, g.cached = 50_000, 0, 0
	assert.Equal(t, *f.stats(t).ShareAvoidedPct, *g.stats(t).ShareAvoidedPct)
}

func TestShareUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		f     shareFixture
		basis string
	}{
		{"no hook tokens", shareFixture{aware: true, noTokens: true}, shareBasisNoTokens},
		{"turns without cache fields", shareFixture{aware: true, fresh: 1000, output: 10, turns: 3, known: 2}, shareBasisCacheFields},
		{"old hook without the counter", shareFixture{aware: true, fresh: 1000, cached: 500, output: 10, turns: 3}, shareBasisCacheFields},
		{"client cannot tell repeats apart", shareFixture{fresh: 1000, output: 10, turns: 1, known: 1}, shareBasisNoRepeats},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.f.stats(t)
			assert.False(t, a.ShareAvoidedKnown)
			assert.Equal(t, a.ShareAvoidedBasis, tc.basis)
			assert.True(t, a.ShareAvoidedPct == nil, "unknown, not 0 and not the raw figure")
			assert.True(t, a.ShareUpperPct == nil)
			raw, err := json.Marshal(a)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"share_avoided_pct":null`)
		})
	}
	// One group of a known client next to one without cache fields: the
	// whole scope is unknown.
	f := known(shareFixture{})
	f.extra = []usage.Bucket{{Day: usage.Day(time.Now()), Org: "o", Repo: "other", Tool: usage.TokenTurns, Client: "codex", Calls: 2},
		{Day: usage.Day(time.Now()), Org: "o", Repo: "other", Tool: usage.TokenInput, Client: "codex", Calls: 100}}
	assert.False(t, f.stats(t).ShareAvoidedKnown)
}

func TestRepeatRangesLowerAvoided(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		f                      shareFixture
		avoided, share, unware int
	}{
		{"no repeats", known(shareFixture{returned: 3}), 3000, 3000, 0},
		{"one repeat in four", known(shareFixture{returned: 3, repeats: 1}), 2250, 2250, 0},
		{"all repeats", known(shareFixture{repeats: 2}), 0, 0, 0},
		{"client without the count", shareFixture{fresh: 1000, output: 10, turns: 1, known: 1, repeats: 5}, 3000, 0, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.f.stats(t)
			assert.Equal(t, a.AvoidedTokens, tc.avoided)
			assert.Equal(t, a.ShareAvoidedTokens, tc.share)
			assert.Equal(t, a.AvoidedUnawareTokens, tc.unware)
		})
	}
	// A second, unaware client adds to the avoided tokens but not the share.
	f := known(shareFixture{})
	f.extra = []usage.Bucket{{Day: usage.Day(time.Now()), Org: "o", Tool: "search", Client: "cursor", Calls: 1, ResponseTokens: 1000}}
	a := f.stats(t)
	assert.Equal(t, a.AvoidedTokens, 6000)
	assert.Equal(t, a.ShareAvoidedTokens, 3000)
	assert.Equal(t, a.AvoidedUnawareTokens, 3000)
	assert.Equal(t, *a.ShareAvoidedPct, *known(shareFixture{}).stats(t).ShareAvoidedPct)
}

func TestShareUpperFigure(t *testing.T) {
	f := known(shareFixture{resend: 4000})
	f.cached = 200_000
	a := f.stats(t)
	// 4,000 answer tokens × turns at 3 avoided per response token.
	assert.Equal(t, a.AvoidedResendTokens, 12_000)
	require.NotNil(t, a.ShareUpperPct)
	assert.True(t, *a.ShareUpperPct >= *a.ShareAvoidedPct)
	assert.Equal(t, a.AvoidedTokens, 3000, "re-sends never join the avoided tokens")
	assert.Equal(t, a.ContextTotalTokens, a.ResponseTokens+3000)

	// Many cache reads and no re-sends: the upper figure is not shown below
	// the main one.
	g := known(shareFixture{})
	g.cached = 5_000_000
	b := g.stats(t)
	assert.Equal(t, *b.ShareUpperPct, *b.ShareAvoidedPct)

	// The attention summary reads the avoided tokens, never the re-sends.
	u := agentUsage(f.statsBuckets(t), []RepoSummary{{Name: "r", GraphTokens: 1, BaseTokens: 4}}, 30, time.Now())
	assert.Equal(t, u.AvoidedTokens, 3000)
}

// statsBuckets rebuilds the fixture's buckets through agentStats' input.
func (f shareFixture) statsBuckets(t *testing.T) []usage.Bucket {
	t.Helper()
	now := time.Now()
	day := usage.Day(now)
	tc := usage.TokenCounts{Input: f.fresh + f.cached + f.write, Output: f.output, CachedInput: f.cached, CacheWrite: f.write, Turns: f.turns, CacheKnownTurns: f.known}
	out := []usage.Bucket{{Day: day, Org: "o", Tool: "search", Client: "claude-code", Calls: 1, ResponseTokens: 1000},
		{Day: day, Org: "o", Repo: "r", Tool: usage.RepeatAware, Client: "claude-code", Calls: 1},
		{Day: day, Org: "o", Repo: "r", Tool: usage.ResendTool("search"), Client: "claude-code", Calls: f.resend}}
	for tool, n := range tc.Counters() {
		out = append(out, usage.Bucket{Day: day, Org: "o", Repo: "r", Tool: tool, Client: "claude-code", Calls: n})
	}
	return out
}

func TestSavedEstimatePricing(t *testing.T) {
	anthropic := PriceReference{InputPerMTok: 5, OutputPerMTok: 25, CachedInputPerMTok: 0.5, CacheWritePerMTok: 6.25, CachesByDefault: true}
	openai := PriceReference{InputPerMTok: 2, OutputPerMTok: 8, CachedInputPerMTok: 0.5}
	a := AgentStats{ShareAvoidedTokens: 3_000_000, ShareLocalAvoidedTokens: 1_000_000, AvoidedResendTokens: 10_000_000}
	for _, tc := range []struct {
		name           string
		a              AgentStats
		ref            PriceReference
		price          string
		dollars, upper *float64
	}{
		{"caches by default: cache-write price", a, anthropic, savedPriceCacheWrite, ptr(12.5), ptr(17.5)},
		{"no default cache: input price", a, openai, savedPriceInput, ptr(4), ptr(9)},
		{"missing cache-write price", a, PriceReference{InputPerMTok: 5, CachesByDefault: true, CachedInputPerMTok: 0.5}, savedPriceCacheWrite, nil, nil},
		{"missing input price", a, PriceReference{CachedInputPerMTok: 0.5}, savedPriceInput, nil, nil},
		{"missing cache-read price: upper unknown", a, PriceReference{InputPerMTok: 2}, savedPriceInput, ptr(4), nil},
		{"only clients without repeats", AgentStats{AvoidedUnawareTokens: 5000}, openai, savedPriceInput, nil, nil},
		{"nothing avoided", AgentStats{}, openai, savedPriceInput, ptr(0), ptr(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := savedEstimate(tc.a, tc.ref)
			assert.Equal(t, e.Price, tc.price)
			assert.Equal(t, e.Dollars, tc.dollars)
			assert.Equal(t, e.UpperDollars, tc.upper)
		})
	}
	// Re-sends never change the main saving.
	b := a
	b.AvoidedResendTokens = 0
	assert.Equal(t, savedEstimate(b, anthropic).Dollars, savedEstimate(a, anthropic).Dollars)
	assert.Equal(t, savedEstimate(a, anthropic).Tokens, 2_000_000, "ask_local is valued with the local models")
}

func TestPriceReference(t *testing.T) {
	ref := (&Server{}).priceReference()
	assert.Equal(t, ref, defaultReference)
	assert.Equal(t, ref.CacheWritePerMTok, 6.25)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "claude", Kind: llm.KindAnthropic, Model: "m", PriceInputPerMTok: 3, PriceOutputPerMTok: 15, PriceCacheWritePerMTok: 3.75, PriceCachedInputPerMTok: 0.3},
	}, Default: "claude"})
	ref = (&Server{cfg: Config{LLM: reg}}).priceReference()
	assert.Equal(t, ref.CacheWritePerMTok, 3.75)
	assert.True(t, ref.CachesByDefault)
	assert.False(t, ref.Estimate)
}

// TestProviderCacheSavingIsNotOurs: the provider's prompt-cache saving
// (llm_savings) is never read into the agent figures or the saving.
func TestProviderCacheSavingIsNotOurs(t *testing.T) {
	for _, q := range []any{modelQueries, gatewayQueries} {
		v := reflect.ValueOf(q)
		for i := range v.NumField() {
			assert.False(t, strings.Contains(v.Field(i).String(), "llm_cost_saved"), v.Type().Field(i).Name)
		}
	}
	// The saving is a function of the agent figures and the price only.
	typ := reflect.TypeOf(savedEstimate)
	assert.Equal(t, typ.NumIn(), 2)
	assert.Equal(t, typ.In(0), reflect.TypeFor[AgentStats]())
	assert.Equal(t, typ.In(1), reflect.TypeFor[PriceReference]())
}
