package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Prices and where they come from. A cost or savings figure is only as good
// as its prices, so each provider names their source (price_source and
// price_checked), or opts into OpenRouter's public model list
// (price_feed: openrouter), which is fetched once a day; nothing about the
// installation is sent, the list is filtered here.

// PriceFeedOpenRouter is the one supported price feed.
const PriceFeedOpenRouter = "openrouter"

// openRouterModels is OpenRouter's public model list with per-token prices
// (a variable for tests).
var openRouterModels = "https://openrouter.ai/api/v1/models"

var (
	dateRe       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	priceModelRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,120}$`)
	// minorRe turns Anthropic's claude-opus-5-5 into OpenRouter's
	// claude-opus-5.5.
	minorRe = regexp.MustCompile(`-(\d+)-(\d+)$`)
)

func validatePriceSource(p ProviderConfig) error {
	if p.PriceSource != "" {
		u, err := url.Parse(p.PriceSource)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(p.PriceSource) > 300 {
			return errors.New("price_source must be an http(s) URL of at most 300 characters")
		}
	}
	if p.PriceChecked != "" {
		if _, err := time.Parse(time.DateOnly, p.PriceChecked); err != nil || !dateRe.MatchString(p.PriceChecked) {
			return errors.New("price_checked must be a date, YYYY-MM-DD")
		}
	}
	switch p.PriceFeed {
	case "", PriceFeedOpenRouter:
	default:
		return fmt.Errorf("price_feed must be empty or %q", PriceFeedOpenRouter)
	}
	if p.PriceModel != "" && !priceModelRe.MatchString(p.PriceModel) {
		return errors.New("price_model must be a model id such as anthropic/claude-opus-5.5")
	}
	if p.PriceFeed != "" && p.Local() {
		return errors.New("price_feed is for cloud providers; local models cost nothing per token")
	}
	return nil
}

// feedID is the model's id in OpenRouter's list.
func feedID(p ProviderConfig) string {
	if p.PriceModel != "" {
		return p.PriceModel
	}
	id := p.Model
	if p.Kind == KindAnthropic && !strings.Contains(id, "/") {
		id = "anthropic/" + minorRe.ReplaceAllString(id, "-$1.$2")
	}
	return id
}

// feedPrices are one model's prices in dollars per million tokens.
type feedPrices struct{ input, output, cacheRead, cacheWrite float64 }

// fetchFeed reads OpenRouter's list into prices by model id.
func fetchFeed(ctx context.Context) (map[string]feedPrices, error) {
	var res struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := getJSON(ctx, "GET", openRouterModels, "", nil, &res); err != nil {
		return nil, err
	}
	perM := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || v > 1 { // more than $1M per million tokens is not a price
			return 0
		}
		return math.Round(v*1e12) / 1e6 // per million tokens, to a millionth of a dollar
	}
	out := make(map[string]feedPrices, len(res.Data))
	for _, m := range res.Data {
		out[m.ID] = feedPrices{perM(m.Pricing.Prompt), perM(m.Pricing.Completion), perM(m.Pricing.InputCacheRead), perM(m.Pricing.InputCacheWrite)}
	}
	return out, nil
}

// RefreshPrices sets the prices of every provider with a price feed from the
// feed, naming it and the day as their source. It returns one line per
// provider it could not price.
func (r *Registry) RefreshPrices(ctx context.Context) []string {
	var feed map[string]feedPrices
	var problems []string
	for _, p := range r.Configs() {
		if p.PriceFeed != PriceFeedOpenRouter {
			continue
		}
		if feed == nil {
			var err error
			if feed, err = fetchFeed(ctx); err != nil {
				return []string{fmt.Sprintf("price feed %s: %v", openRouterModels, err)}
			}
		}
		fp, ok := feed[feedID(p)]
		if !ok || fp.input <= 0 || fp.output <= 0 {
			problems = append(problems, fmt.Sprintf("%s: %s is not in the price feed; set price_model", p.Name, feedID(p)))
			continue
		}
		r.mu.Lock()
		for i := range r.cfg.Providers {
			c := &r.cfg.Providers[i]
			if c.Name != p.Name {
				continue
			}
			c.PriceInputPerMTok, c.PriceOutputPerMTok = fp.input, fp.output
			c.PriceCachedInputPerMTok, c.PriceCacheWritePerMTok = fp.cacheRead, fp.cacheWrite
			c.PricingConfigured = true
			c.PriceSource = openRouterModels + "#" + feedID(p)
			c.PriceChecked = time.Now().UTC().Format(time.DateOnly)
		}
		r.mu.Unlock()
	}
	return problems
}

// RefreshPricesEvery refreshes the feed prices every interval until ctx
// ends, reporting each problem. The caller refreshes once at start-up.
func (r *Registry) RefreshPricesEvery(ctx context.Context, interval time.Duration, report func(problem string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		for _, p := range r.RefreshPrices(rctx) {
			report(p)
		}
		cancel()
	}
}

// HasPriceFeed reports whether any provider takes its prices from a feed.
func (r *Registry) HasPriceFeed() bool {
	for _, p := range r.Configs() {
		if p.PriceFeed != "" {
			return true
		}
	}
	return false
}
