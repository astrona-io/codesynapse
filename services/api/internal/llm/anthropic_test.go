package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
}
