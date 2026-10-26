package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sse writes one server-sent event.
func sse(w io.Writer, data string) {
	_, _ = io.WriteString(w, "data: "+data+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// A local answer is streamed: the time to the first token (thinking
// counts) splits reading from writing, and both are recorded with the
// share of the context window the request used.
func TestLocalAnswersAreStreamedAndTimed(t *testing.T) {
	var asked map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&asked))
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(40 * time.Millisecond) // reading the prompt
		sse(w, `{"model":"qwen","choices":[{"delta":{"reasoning":"hmm"}}]}`)
		time.Sleep(20 * time.Millisecond)
		sse(w, `{"choices":[{"delta":{"content":"Hello "}}]}`)
		sse(w, `{"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}]}`)
		sse(w, `{"choices":[],"usage":{"prompt_tokens":300,"completion_tokens":20}}`)
		sse(w, `[DONE]`)
	}))
	t.Cleanup(srv.Close)
	tenant := strings.Repeat("c", 24)
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "stream", Kind: KindOpenAI, BaseURL: srv.URL, Model: "qwen", ContextWindow: 32000, MaxOutputTokens: 1000, TimeoutSeconds: 5}}})
	resp, err := r.Complete(WithTask(telemetry.WithTenant(modelContext(t), tenant), "review"), "stream", Request{User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, asked["stream"], true)
	assert.Equal[any](t, asked["stream_options"], map[string]any{"include_usage": true})
	assert.Equal(t, resp.Text, "Hello world")
	assert.Equal(t, []int{resp.InputTokens, resp.OutputTokens}, []int{300, 20})
	assert.True(t, resp.FirstToken >= 35*time.Millisecond, resp.FirstToken.String())
	assert.True(t, resp.Duration > resp.FirstToken)

	assert.Equal(t, metricTotal(t, "llm_read_tokens_total", tenant), 300.0)
	assert.Equal(t, metricTotal(t, "llm_write_tokens_total", tenant), 20.0)
	assert.True(t, metricTotal(t, "llm_read_seconds_total", tenant) >= 0.035)
	assert.True(t, metricTotal(t, "llm_write_seconds_total", tenant) > 0)
	assert.Equal(t, metricTotal(t, "llm_context_used_ratio_count", tenant), 1.0, "one request observed")
}

// A server that refuses the streaming options is asked whole, then and
// from then on.
func TestStreamRefusedFallsBackToWhole(t *testing.T) {
	var calls, streamed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		if body["stream"] == true {
			streamed.Add(1)
			http.Error(w, `{"error":"unknown field stream_options"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "old", Kind: KindOpenAI, BaseURL: srv.URL, Model: "m", TimeoutSeconds: 5}}})
	for range 2 {
		resp, err := r.Complete(modelContext(t), "old", Request{User: "hi"})
		require.NoError(t, err)
		assert.Equal(t, resp.Text, "ok")
		assert.Equal(t, resp.FirstToken, time.Duration(0))
	}
	assert.Equal(t, streamed.Load(), int32(1), "asked to stream once")
	assert.Equal(t, calls.Load(), int32(3))
}

// A stream that reports an error fails the request instead of answering
// with what came before it.
func TestStreamErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, `{"choices":[{"delta":{"content":"par"}}]}`)
		sse(w, `{"error":{"message":"model unloaded"}}`)
	}))
	t.Cleanup(srv.Close)
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "broken", Kind: KindOpenAI, BaseURL: srv.URL, Model: "m", TimeoutSeconds: 5}}})
	_, err := r.Complete(modelContext(t), "broken", Request{User: "hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model unloaded")
}
