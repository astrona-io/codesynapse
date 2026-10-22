package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestAnthropicCachesTheSystemPrompt(t *testing.T) {
	var body struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":40,"cache_read_input_tokens":2000,"cache_creation_input_tokens":0,"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		}
	}))
	t.Cleanup(srv.Close)

	p := newAnthropic(ProviderConfig{Name: "claude", Model: "claude-opus-5-5", BaseURL: srv.URL, apiKey: "sk-test"})
	resp, err := p.Complete(context.Background(), Request{System: "You triage findings.", User: "finding 1", MaxTokens: 100})
	require.NoError(t, err)
	assert.Equal(t, resp.Text, "OK")
	require.Len(t, body.System, 1)
	assert.Equal(t, string(body.System[0].CacheControl), `{"type":"ephemeral"}`, "the system prompt is the cache prefix")
	require.Len(t, body.Messages, 1)
	_, cached := body.Messages[0].Content[0]["cache_control"]
	assert.False(t, cached, "the per-call user part is not")
	assert.Equal(t, resp.CachedInputTokens, 2000)
	assert.Equal(t, resp.InputTokens, 2040, "cache reads count as input")
	assert.True(t, resp.UsageKnown)
}

func TestAnthropicRetryAndInterruptedUsage(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) {
			attempts, consumed := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				consumed += len(data)
				attempts++
				if attempts == 1 {
					w.Header().Set("retry-after", "0")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"retry"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"configured\",\"content\":[],\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":30,\"output_tokens\":1}}}\n\n")
				if interrupted {
					_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"interrupted\"}}\n\n")
				} else {
					_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			t.Cleanup(srv.Close)
			cfg := ProviderConfig{Name: "retry", Model: "configured", apiKey: "test"}
			p := &anthropicProvider{cfg: cfg, client: anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(1), option.WithHTTPClient(&http.Client{Transport: measuredTransport{next: http.DefaultTransport}}))}
			telemetry.AllowModels("retry", "configured")
			tenant := strings.Repeat("b", 24)
			if interrupted {
				tenant = strings.Repeat("c", 24)
			}
			ctx := telemetry.WithModel(telemetry.WithTenant(t.Context(), tenant), "retry", "configured")
			before := metricTotal(t, "cloud_bytes_sent_total", tenant)
			resp, err := p.Complete(ctx, Request{User: "hello", MaxTokens: 20})
			if interrupted {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, attempts, 2)
			assert.Equal(t, metricTotal(t, "cloud_bytes_sent_total", tenant)-before, float64(consumed))
			assert.Equal(t, resp.InputTokens, 60)
			assert.Equal(t, resp.CachedInputTokens, 20)
			assert.Equal(t, resp.CacheWriteInputTokens, 30)
			assert.Equal(t, resp.UsageKnown, !interrupted)
			assert.True(t, resp.UsageReported)
		})
	}
}

func TestAnthropicMalformedUsageIsUnknown(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":-1,"output_tokens":1}`,
		`{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"output_tokens":1}`,
		`{"input_tokens":1,"cache_creation_input_tokens":-1,"output_tokens":1}`,
	} {
		t.Run(usage, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bad\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"configured\",\"content\":[],\"usage\":%s}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", usage)
			}))
			t.Cleanup(srv.Close)
			p := newAnthropic(ProviderConfig{Name: "invalid", Model: "configured", BaseURL: srv.URL, apiKey: "test"})
			resp, err := p.Complete(t.Context(), Request{User: "hello", MaxTokens: 10})
			require.NoError(t, err)
			assert.False(t, resp.UsageKnown)
			assert.False(t, resp.UsageReported)
			assert.Equal(t, resp.CachedInputTokens, 0)
		})
	}
}
