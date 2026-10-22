package llm

import (
	"math"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
)

func TestCost(t *testing.T) {
	cfg := ProviderConfig{PriceInputPerMTok: 3, PriceCachedInputPerMTok: 0.3, PriceOutputPerMTok: 15}
	// 1M prompt tokens of which 400k cached, 100k output:
	// 600k*3 + 400k*0.3 + 100k*15 = 1.8 + 0.12 + 1.5 dollars.
	got := Cost(cfg, Response{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000})
	assert.True(t, math.Abs(got-3.42) < 1e-9, "got %v", got)
	cfg.PriceCachedInputPerMTok = 0
	got = Cost(cfg, Response{InputTokens: 1_000_000, CachedInputTokens: 400_000})
	assert.True(t, math.Abs(got-3) < 1e-9, "cached input falls back to the input price")
	assert.Equal(t, Cost(ProviderConfig{}, Response{InputTokens: 5000, OutputTokens: 5000}), 0.0, "local models without prices are free")
}
