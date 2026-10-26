package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type budgetProvider struct{ calls atomic.Int64 }

func (p *budgetProvider) Complete(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return Response{Text: "ok", InputTokens: 7, OutputTokens: 2, UsageKnown: true, UsageReported: true}, nil
}

func (*budgetProvider) Models(context.Context) ([]string, error) { return nil, nil }

func budgetRegistry(cfg ProviderConfig) (*Registry, *budgetProvider) {
	cfg.Name = "budget"
	r := NewRegistry(Config{Providers: []ProviderConfig{cfg}, Default: cfg.Name})
	p := &budgetProvider{}
	r.impl[cfg.Name] = p
	return r, p
}

func TestFinalPromptBoundaryAndUsage(t *testing.T) {
	ctx := modelContext(t)
	req := Request{System: "System instruction", User: "func λ() { println(\"日本語 <>&\\\") }\n<|endoftext|>", MaxTokens: 20}
	r, _ := budgetRegistry(ProviderConfig{Model: "unverified-local", MaxOutputTokens: 20})
	count, err := r.Preflight(ctx, "budget", req)
	require.NoError(t, err)
	assert.False(t, count.Exact)
	assert.Equal(t, count.Model, "unverified-local")
	assert.Equal(t, count.Encoding, "utf8-bytes")
	assert.Equal(t, count.Version, "request-json-v1")
	assert.Greater(t, count.Tokens, len(req.System)+len(req.User))
	assert.Equal(t, r.Usage("budget").Requests, 0)
	for _, delta := range []int{-1, 0, 1} {
		t.Run(string(rune('b'+delta)), func(t *testing.T) {
			r, p := budgetRegistry(ProviderConfig{Model: "unverified-local", MaxOutputTokens: 20, MaxInputTokens: count.Tokens + delta})
			resp, err := r.Complete(ctx, "budget", req)
			if delta < 0 {
				require.ErrorIs(t, err, ErrInputBudget)
				assert.Equal(t, p.calls.Load(), int64(0))
				assert.Equal(t, r.Usage("budget").Requests, 0)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, p.calls.Load(), int64(1))
			assert.Equal(t, resp.Preflight, count)
			assert.Equal(t, r.Usage("budget").InputTokens, 7)
			assert.Equal(t, resp.InputTokens, 7)
			assert.True(t, resp.UsageKnown)
		})
	}
}

func TestPreflightRedactsBeforeCounting(t *testing.T) {
	ctx := modelContext(t)
	req := Request{System: "constant", User: "123-45-6789", MaxTokens: 20}
	cfg := ProviderConfig{Model: "m", MaxOutputTokens: 20}
	raw, err := json.Marshal(openAIRequest(cfg, req))
	require.NoError(t, err)
	cfg.MaxInputTokens = len(raw)
	r, p := budgetRegistry(cfg)
	count, err := r.Preflight(ctx, "budget", req)
	require.ErrorIs(t, err, ErrInputBudget)
	assert.Greater(t, count.Tokens, len(raw), "redaction marker expands this fixture")
	assert.False(t, strings.Contains(err.Error(), req.User))
	_, err = r.Complete(ctx, "budget", req)
	require.ErrorIs(t, err, ErrInputBudget)
	assert.Equal(t, p.calls.Load(), int64(0))
}

func TestPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ProviderConfig
		req  Request
		want error
	}{
		{"strict unknown model", ProviderConfig{InputBudgetPolicy: "require_exact", Model: "unknown"}, Request{User: "hi"}, ErrExactCountUnavailable},
		{"strict named OpenAI model", ProviderConfig{InputBudgetPolicy: "require_exact", Model: "gpt-4o"}, Request{User: "hi"}, ErrExactCountUnavailable},
		{"combined raw bytes", ProviderConfig{}, Request{System: "s", User: strings.Repeat("x", MaxPromptBytes)}, ErrInputBudget},
		{"escaping expansion", ProviderConfig{MaxInputTokens: 10 << 20}, Request{User: strings.Repeat("\x00", MaxPromptBytes)}, ErrInputBudget},
		{"invalid UTF8", ProviderConfig{}, Request{User: string([]byte{0xff})}, nil},
		{"negative budget", ProviderConfig{MaxInputTokens: -1}, Request{}, nil},
		{"tiny window", ProviderConfig{ContextWindow: 100}, Request{}, nil},
		{"unknown policy", ProviderConfig{InputBudgetPolicy: "guess"}, Request{}, nil},
		{"long model", ProviderConfig{Model: strings.Repeat("x", 4097)}, Request{}, nil},
		{"long effort", ProviderConfig{Effort: strings.Repeat("x", 65)}, Request{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := budgetRegistry(tc.cfg)
			_, err := r.Complete(modelContext(t), "budget", tc.req)
			require.Error(t, err)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
			}
			assert.Equal(t, p.calls.Load(), int64(0))
			assert.Equal(t, r.Usage("budget").Requests, 0)
		})
	}
}

func TestPreflightIdentityCancellationAndConfiguration(t *testing.T) {
	r, p := budgetRegistry(ProviderConfig{Model: "original"})
	_, err := r.Preflight(t.Context(), "budget", Request{})
	require.ErrorIs(t, err, execution.ErrMissing)
	ctx, cancel := context.WithCancel(modelContext(t))
	cancel()
	_, err = r.Complete(ctx, "budget", Request{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, p.calls.Load(), int64(0))
	cfg := Config{Providers: []ProviderConfig{{Name: "fixed", Kind: KindOpenAI, Model: "original"}}, Default: "fixed", Tasks: map[string]string{"review": "fixed"}}
	r = NewRegistry(cfg)
	cfg.Providers[0].Model = "mutated"
	cfg.Tasks["review"] = "mutated"
	count, err := r.Preflight(modelContext(t), "fixed", Request{User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, count.Model, "original")
	assert.Equal(t, r.Routes().Tasks["review"], "fixed")
}

func TestPreflightContextReservesOutput(t *testing.T) {
	cfg := ProviderConfig{ContextWindow: 4096, MaxInputTokens: 4000, MaxOutputTokens: 1024}
	r, p := budgetRegistry(cfg)
	effective, ok := r.Config("budget")
	require.True(t, ok)
	assert.Equal(t, effective.MaxInputTokens, 2048)
	_, err := r.Complete(modelContext(t), "budget", Request{User: strings.Repeat("x", 2048), MaxTokens: 9999})
	require.ErrorIs(t, err, ErrInputBudget)
	assert.Equal(t, p.calls.Load(), int64(0))
}

func TestPreflightCanonicalProviderRequests(t *testing.T) {
	for _, kind := range []string{KindOpenAI, KindAnthropic} {
		t.Run(kind, func(t *testing.T) {
			var received map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
				if kind == KindOpenAI {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"configured\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			t.Cleanup(srv.Close)
			cfg := ProviderConfig{Name: "wire", Kind: kind, BaseURL: srv.URL, Model: "configured", apiKey: "test", MaxOutputTokens: 30, Effort: "medium", DisableThinking: true}
			r := NewRegistry(Config{Providers: []ProviderConfig{cfg}})
			req := Request{System: "fixed <>&\n", User: "日本語\n123-45-6789 <|endoftext|> \\\"", MaxTokens: 999}
			resp, err := r.Complete(modelContext(t), "wire", req)
			require.NoError(t, err)
			canonical, err := json.Marshal(received)
			require.NoError(t, err)
			assert.Equal(t, resp.Preflight.Bytes, len(canonical), "counter uses the provider's complete request shape")
			assert.False(t, strings.Contains(string(canonical), "123-45-6789"))
			assert.Equal(t, resp.InputTokens, 3)
		})
	}
}

type fixedCounter struct{ count InputCount }

func (c fixedCounter) Count(context.Context, ProviderConfig, Request) (InputCount, error) {
	return c.count, nil
}

func TestPreflightRejectsInvalidCounterResult(t *testing.T) {
	for _, count := range []InputCount{
		{Tokens: -1, Model: "m", Encoding: "e", Version: "v", Exact: true},
		{Bytes: -1, Model: "m", Encoding: "e", Version: "v", Exact: true},
		{Model: "other", Encoding: "e", Version: "v", Exact: true},
		{Model: "m", Exact: true},
		{Bytes: maxPromptEnvelopeBytes + 1, Model: "m", Encoding: "e", Version: "v", Exact: true},
	} {
		r, p := budgetRegistry(ProviderConfig{Model: "m", InputBudgetPolicy: "require_exact"})
		r.counter = fixedCounter{count: count}
		_, err := r.Complete(modelContext(t), "budget", Request{User: "hi"})
		require.Error(t, err)
		assert.Equal(t, p.calls.Load(), int64(0))
	}
}

func TestPreflightConcurrent(t *testing.T) {
	r, p := budgetRegistry(ProviderConfig{})
	ctx := modelContext(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _, err := r.Complete(ctx, "budget", Request{User: "hello"}); assert.NoError(t, err) })
	}
	wg.Wait()
	assert.Equal(t, p.calls.Load(), int64(20))
	assert.Equal(t, r.Usage("budget").InputTokens, 140)
}

func FuzzPromptPreflight(f *testing.F) {
	f.Add("system", "hello", 10000)
	f.Add("", "\x00日本語<|endoftext|>", 1)
	f.Fuzz(func(t *testing.T, system, user string, limit int) {
		if len(system) > MaxPromptBytes || len(user) > MaxPromptBytes {
			t.Skip()
		}
		r, p := budgetRegistry(ProviderConfig{MaxInputTokens: limit, Model: "fixture"})
		count, err := r.Preflight(modelContext(t), "budget", Request{System: system, User: user})
		assert.Equal(t, p.calls.Load(), int64(0))
		if err == nil {
			assert.True(t, count.Tokens >= 0 && count.Bytes <= maxPromptEnvelopeBytes)
			assert.False(t, count.Exact)
		}
	})
}

func TestBytesPerTokenScalesTheEstimate(t *testing.T) {
	cfg := ProviderConfig{Name: "p", Kind: KindOpenAI, Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}
	req := Request{System: "s", User: strings.Repeat("word ", 400)}
	base, err := requestByteCounter{}.Count(t.Context(), cfg, req)
	require.NoError(t, err)
	assert.Equal(t, base.Tokens, base.Bytes, "default counts one byte as one token")
	cfg.BytesPerToken = 3
	three, err := requestByteCounter{}.Count(t.Context(), cfg, req)
	require.NoError(t, err)
	assert.Equal(t, three.Bytes, base.Bytes)
	assert.Equal(t, three.Tokens, (base.Bytes+2)/3)
	cfg.BytesPerToken = 9
	assert.Error(t, validateBudgetConfig(cfg))
}

func TestOpenAIRequestReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ProviderConfig
		want any
	}{
		{"unset", ProviderConfig{DisableThinking: true}, nil},
		{"none", ProviderConfig{Effort: "none"}, "none"},
		{"high", ProviderConfig{Effort: "high"}, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := openAIRequest(tc.cfg, Request{User: "hi"})
			assert.Equal(t, body["reasoning_effort"], tc.want)
		})
	}
}
