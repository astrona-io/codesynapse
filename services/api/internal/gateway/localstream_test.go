package gateway

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The testdata/ollama-*.sse streams were recorded from Ollama 0.40.12
// (qwen3.8:27b) on /v1/chat/completions with synthetic prompts:
//
//   - effort-none: "Say hello in three words.", reasoning_effort "none".
//   - thinking-length: the same prompt with no reasoning_effort and
//     max_tokens 60: every token went to "reasoning", finish_reason "length".
//   - tool-call: a get_weather tool, reasoning_effort "none": the whole call
//     in one delta, then finish_reason "tool_calls".
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

const (
	weatherFn = `{"name":"get_weather","description":"Weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`
	// ollamaBrokeOff is what Ollama's chat endpoint sends when generation
	// fails after the 200: the error becomes an empty delta and the stream
	// stops with no finish_reason and no [DONE].
	ollamaBrokeOff = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}` + "\n\n"
	// streamError is how vLLM and llama.cpp report an error after the 200.
	streamError = `data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n" +
		`data: {"error":{"message":"out of memory","type":"server_error"}}` + "\n\n"
)

func TestLocalStreamEndings(t *testing.T) {
	big := func(maxTokens string, tools bool) string {
		b := `{"model":"claude-opus-5-5","max_tokens":` + maxTokens + `,"stream":true,"messages":[{"role":"user","content":"hi"}]`
		if tools {
			b += `,"tools":[` + weatherFn + `]`
		}
		return b + "}"
	}
	small := `{"model":"claude-haiku-4-5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	cases := map[string]struct {
		stream string
		body   string
		reason string // "" when answered locally
		want   []string
	}{
		"effort none answers":        {fixture(t, "ollama-effort-none.sse"), big("32000", false), "", []string{"Hello", " to you", `"stop_reason":"end_turn"`}},
		"thinking used max_tokens":   {fixture(t, "ollama-thinking-length.sse"), big("60", false), FallTruncated, nil},
		"tool call":                  {fixture(t, "ollama-tool-call.sse"), big("32000", true), "", []string{`"name":"get_weather"`, `\"city\":\"Paris\"`, `"stop_reason":"tool_use"`}},
		"ollama broke off":           {ollamaBrokeOff, big("32000", false), FallModelError, nil},
		"ollama broke off, tools":    {ollamaBrokeOff, big("32000", true), FallModelError, nil},
		"error in the stream":        {streamError, big("32000", false), FallModelError, nil},
		"finish without [DONE]":      {`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n", big("32000", false), "", []string{"ok"}},
		"[DONE] without finish":      {`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\ndata: [DONE]\n\n", big("32000", false), FallTruncated, nil},
		"small model, error first":   {`data: {"error":{"message":"model failed to load"}}` + "\n\n", small, FallModelError, nil},
		"small model, empty body":    {"", small, FallModelError, nil},
		"small model, effort none":   {fixture(t, "ollama-effort-none.sse"), small, "", []string{"Hello", " you", `"stop_reason":"end_turn"`}},
		"small model, no [DONE] yet": {ollamaBrokeOff, small, "", []string{"Hel"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.stream) }))
			defer srv.Close()
			l := &Local{BaseURL: srv.URL + "/v1", Model: "qwen3.8:27b", Effort: "none"}
			rec := httptest.NewRecorder()
			_, reason, ok := l.serve(t.Context(), rec, []byte(tc.body))
			assert.Equal(t, reason, tc.reason)
			assert.Equal(t, ok, tc.reason == "")
			if !ok {
				assert.Equal(t, rec.Body.Len(), 0)
				return
			}
			for _, w := range tc.want {
				assert.Contains(t, rec.Body.String(), w)
			}
		})
	}
}

func TestStreamReaderEnd(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		done, failed bool
	}{
		{"done", fixture(t, "ollama-effort-none.sse"), true, false},
		{"broke off", ollamaBrokeOff, false, false},
		{"error object", streamError, false, true},
		{"error null is no error", `data: {"error":null,"choices":[{"delta":{"content":"x"}}]}` + "\n\ndata: [DONE]\n\n", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &streamReader{sc: bufio.NewScanner(strings.NewReader(tc.stream))}
			chunks := 0
			for _, ok := r.next(); ok; _, ok = r.next() {
				chunks++
			}
			assert.True(t, chunks > 0)
			assert.Equal(t, r.done, tc.done)
			assert.Equal(t, r.failed, tc.failed)
			_, ok := r.next()
			assert.False(t, ok)
		})
	}
}
