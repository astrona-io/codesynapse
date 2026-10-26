package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPriceSourceValidation(t *testing.T) {
	cloud := ProviderConfig{Name: "claude", Kind: KindAnthropic}
	for _, tc := range []struct {
		name string
		set  func(*ProviderConfig)
		ok   bool
	}{
		{"source and date", func(p *ProviderConfig) {
			p.PriceSource, p.PriceChecked = "https://platform.claude.com/docs/en/about-claude/pricing", "2026-10-26"
		}, true},
		{"feed", func(p *ProviderConfig) { p.PriceFeed = PriceFeedOpenRouter }, true},
		{"not a URL", func(p *ProviderConfig) { p.PriceSource = "the website" }, false},
		{"script URL", func(p *ProviderConfig) { p.PriceSource = "javascript:alert(1)" }, false},
		{"bad date", func(p *ProviderConfig) { p.PriceChecked = "26/10/2026" }, false},
		{"unknown feed", func(p *ProviderConfig) { p.PriceFeed = "litellm" }, false},
		{"bad model id", func(p *ProviderConfig) { p.PriceModel = "claude opus" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := cloud
			tc.set(&p)
			var c Config
			err := c.Add(p, func(string) string { return "" })
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
	var c Config
	assert.Error(t, c.Add(ProviderConfig{Name: "local", BaseURL: "http://127.0.0.1:11434/v1", PriceFeed: PriceFeedOpenRouter}, func(string) string { return "" }),
		"local models need no price feed")
}

func TestFeedID(t *testing.T) {
	assert.Equal(t, feedID(ProviderConfig{Kind: KindAnthropic, Model: "claude-opus-5-5"}), "anthropic/claude-opus-5.5")
	assert.Equal(t, feedID(ProviderConfig{Kind: KindAnthropic, Model: "claude-haiku-5-5", PriceModel: "anthropic/claude-haiku-5.5:batch"}), "anthropic/claude-haiku-5.5:batch")
	assert.Equal(t, feedID(ProviderConfig{Kind: KindOpenAI, Model: "openai/gpt-6"}), "openai/gpt-6")
}

// A provider with the feed takes its prices from it and names it as their
// source; one the feed lacks keeps its prices and is reported.
func TestRefreshPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"anthropic/claude-sonnet-5.5","pricing":{"prompt":"0.000002","completion":"0.00001","input_cache_read":"0.0000002"}},{"id":"bogus","pricing":{"prompt":"x","completion":"5"}}]}`)
	}))
	t.Cleanup(srv.Close)
	old := openRouterModels
	openRouterModels = srv.URL
	t.Cleanup(func() { openRouterModels = old })

	var c Config
	require.NoError(t, c.Add(ProviderConfig{Name: "sonnet", Kind: KindAnthropic, Model: "claude-sonnet-5-5", PriceFeed: PriceFeedOpenRouter}, func(string) string { return "" }))
	require.NoError(t, c.Add(ProviderConfig{Name: "other", Kind: KindAnthropic, Model: "claude-unknown-1-0", PriceFeed: PriceFeedOpenRouter, PriceInputPerMTok: 1, PriceOutputPerMTok: 2}, func(string) string { return "" }))
	require.NoError(t, c.Add(ProviderConfig{Name: "manual", Kind: KindAnthropic, Model: "claude-opus-5-5", PriceInputPerMTok: 5, PriceOutputPerMTok: 25}, func(string) string { return "" }))
	r := NewRegistry(c)
	assert.True(t, r.HasPriceFeed())
	problems := r.RefreshPrices(t.Context())
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "anthropic/claude-unknown-1.0 is not in the price feed")

	s, _ := r.Config("sonnet")
	assert.Equal(t, []float64{s.PriceInputPerMTok, s.PriceOutputPerMTok, s.PriceCachedInputPerMTok}, []float64{2, 10, 0.2})
	assert.True(t, s.PricingConfigured)
	assert.Equal(t, s.PriceSource, srv.URL+"#anthropic/claude-sonnet-5.5")
	assert.Equal(t, s.PriceChecked, time.Now().UTC().Format(time.DateOnly))
	o, _ := r.Config("other")
	assert.Equal(t, []float64{o.PriceInputPerMTok, o.PriceOutputPerMTok}, []float64{1, 2}, "kept when the feed lacks it")
	m, _ := r.Config("manual")
	assert.Equal(t, m.PriceSource, "", "providers without the feed are left alone")
}
