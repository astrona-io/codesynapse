package llm

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func metricTotal(t *testing.T, name, tenant string) float64 {
	t.Helper()
	var out strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&out))
	var total float64
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, name+"{") && strings.Contains(line, `tenant_id="`+tenant+`"`) {
			v, err := strconv.ParseFloat(strings.Fields(line)[1], 64)
			require.NoError(t, err)
			total += v
		}
	}
	return total
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCloudTransportCountsConsumedAttempts(t *testing.T) {
	const tenant = "aaaaaaaaaaaaaaaaaaaaaaaa"
	telemetry.AllowModels("test-cloud", "configured")
	ctx := telemetry.WithModel(telemetry.WithTenant(t.Context(), tenant), "test-cloud", "configured")
	for _, tc := range []struct {
		name, url string
		want      float64
	}{
		{"cloud", "https://cloud.example", 7},
		{"local", "http://localhost:9000", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := metricTotal(t, "cloud_bytes_sent_total", tenant)
			next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				_, err := io.CopyN(io.Discard, r.Body, 7)
				require.NoError(t, err)
				return nil, context.Canceled
			})
			transport := modelTransport(ProviderConfig{BaseURL: tc.url}, next)
			for range 2 {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.url, strings.NewReader("encoded request payload"))
				require.NoError(t, err)
				res, err := transport.RoundTrip(req)
				if res != nil {
					require.NoError(t, res.Body.Close())
				}
				require.True(t, errors.Is(err, context.Canceled))
			}
			assert.Equal(t, metricTotal(t, "cloud_bytes_sent_total", tenant)-before, 2*tc.want)
		})
	}
}

func TestCloudTransportDoesNotDoubleCountReplayedBodies(t *testing.T) {
	const tenant = "abababababababababababab"
	telemetry.AllowModels("redirect", "configured")
	ctx := telemetry.WithModel(telemetry.WithTenant(t.Context(), tenant), "redirect", "configured")
	before := metricTotal(t, "cloud_bytes_sent_total", tenant)
	var replay io.ReadCloser
	transport := measuredTransport{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		if replay == nil {
			replay, err = r.GetBody()
			require.NoError(t, err)
		}
		return nil, context.Canceled
	})}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://cloud.example", strings.NewReader("payload"))
	require.NoError(t, err)
	for range 2 {
		res, roundErr := transport.RoundTrip(req)
		if res != nil {
			require.NoError(t, res.Body.Close())
		}
		require.True(t, errors.Is(roundErr, context.Canceled))
		req.Body = replay
	}
	assert.Equal(t, metricTotal(t, "cloud_bytes_sent_total", tenant)-before, 14.0)
}

func TestProviderUsageAndCostCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, usage           string
		known                 bool
		input, cached, output int
	}{
		{"cache", `,"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}`, true, 100, 80, 10},
		{"omitted", "", false, 0, 0, 0},
		{"partial", `,"usage":{"prompt_tokens":40}`, false, 40, 0, 0},
		{"invalid counts", `,"usage":{"prompt_tokens":-1,"completion_tokens":10}`, false, 0, 0, 10},
		{"invalid cache", `,"usage":{"prompt_tokens":5,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":100}}`, false, 5, 0, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"model":"spoofed-model","choices":[{"message":{"content":"ok"}}]`+tc.usage+`}`)
			}))
			t.Cleanup(srv.Close)
			cfg := ProviderConfig{Name: "coverage", Model: "configured", BaseURL: srv.URL, TimeoutSeconds: 5, PriceInputPerMTok: 3, PriceCachedInputPerMTok: 0.3, PriceOutputPerMTok: 15}
			r := NewRegistry(Config{Providers: []ProviderConfig{cfg}})
			resp, err := r.Complete(telemetry.WithTenant(modelContext(t), strings.Repeat(strconv.Itoa(tc.input/100+1), 24)), "coverage", Request{User: "hello"})
			require.NoError(t, err)
			assert.Equal(t, resp.UsageKnown, tc.known)
			assert.Equal(t, resp.InputTokens, tc.input)
			assert.Equal(t, resp.CachedInputTokens, tc.cached)
			assert.Equal(t, resp.OutputTokens, tc.output)
			var out strings.Builder
			require.NoError(t, telemetry.Default.WriteText(&out))
			assert.False(t, strings.Contains(out.String(), `llm_model="spoofed-model"`))
		})
	}
}

func TestPricingCompleteness(t *testing.T) {
	resp := Response{InputTokens: 100, CacheWriteInputTokens: 40, CachedInputTokens: 20, OutputTokens: 10}
	cfg := ProviderConfig{BaseURL: "https://cloud.example", PriceInputPerMTok: 3, PriceCachedInputPerMTok: 0.3, PriceOutputPerMTok: 15}
	assert.False(t, pricesKnown(cfg, resp), "cache write rate missing")
	cfg.PriceCacheWritePerMTok = 3.75
	assert.True(t, pricesKnown(cfg, resp))
	assert.True(t, Cost(cfg, resp) > 0.000425 && Cost(cfg, resp) < 0.000427)
	assert.False(t, pricesKnown(ProviderConfig{}, resp), "unspecified cloud prices remain unknown")
	assert.True(t, pricesKnown(ProviderConfig{PricingConfigured: true}, resp), "explicit free rates")
	assert.True(t, pricesKnown(ProviderConfig{BaseURL: "http://localhost:9999"}, resp), "local token billing is free")
}

func TestUnknownUsageOrPricesDoNotPublishZeroCost(t *testing.T) {
	telemetry.AllowModels("cost-coverage", "configured")
	for i, tc := range []struct {
		name                          string
		known, priced                 bool
		cost, saved, unknown, missing float64
	}{
		{"cache", true, true, 0.000234, 0.000216, 0, 0},
		{"missing prices", true, false, 0, 0, 0, 1},
		{"partial usage", false, true, 0, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant := strings.Repeat(strconv.Itoa(i+6), 24)
			cfg := ProviderConfig{Model: "configured", BaseURL: "https://cloud.example"}
			if tc.priced {
				cfg.PriceInputPerMTok, cfg.PriceCachedInputPerMTok, cfg.PriceOutputPerMTok = 3, 0.3, 15
			}
			ctx := telemetry.WithTenant(t.Context(), tenant)
			resp := Response{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 10, UsageKnown: tc.known, UsageReported: true}
			record(ctx, "cost-coverage", cfg, resp, nil)
			assert.True(t, math.Abs(metricTotal(t, "llm_cost_estimated_dollars", tenant)-tc.cost) < 1e-12)
			assert.True(t, math.Abs(metricTotal(t, "llm_cost_saved_estimated_dollars_total", tenant)-tc.saved) < 1e-12)
			assert.Equal(t, metricTotal(t, "llm_usage_unknown_total", tenant), tc.unknown)
			assert.Equal(t, metricTotal(t, "llm_pricing_missing_total", tenant), tc.missing)
		})
	}
}

func TestLocalClassificationUsesHost(t *testing.T) {
	for _, url := range []string{"https://localhost.evil.example", "https://10.example.com", "https://api.example/127.0.0.1", "https://172.33.1.1"} {
		assert.False(t, (ProviderConfig{BaseURL: url}).Local(), url)
	}
	for _, url := range []string{"http://[::1]:1234", "http://172.16.0.2", "http://model.local:9000"} {
		assert.True(t, (ProviderConfig{BaseURL: url}).Local(), url)
	}
}
