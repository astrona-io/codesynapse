package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestStableSystem(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no header", "You are a helper.", "You are a helper."},
		{"header block first", "x-anthropic-billing-header: cc_version=1; cc_prompt_index=3;\n\nYou are a helper.", "You are a helper."},
		{"header only", "x-anthropic-billing-header: cc_version=1;", ""},
		{"header text later is kept", "You are a helper.\n\nx-anthropic-billing-header: x", "You are a helper.\n\nx-anthropic-billing-header: x"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, stableSystem(tc.in), tc.want) })
	}
}

// Two turns that differ only in Claude Code's per-turn billing header give the
// local server the same bytes, so its prompt cache of the first serves the
// second.
func TestLocalPrefixIsByteStable(t *testing.T) {
	turn := func(n string) []byte {
		return []byte(`{"model":"claude-sonnet-5","max_tokens":64,"system":[` +
			`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cc_prompt_index=` + n + `; cc_prev_req=req_` + n + `;"},` +
			`{"type":"text","text":"You are a coding agent.","cache_control":{"type":"ephemeral"}}],` +
			`"tools":[` + weatherTool + `,{"name":"b_tool","description":"B","input_schema":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}}],` +
			`"messages":[{"role":"user","content":"hello"}]}`)
	}
	l := &Local{Model: "q", Options: map[string]any{"keep_alive": "15m0s"}}
	_, first, reason := l.translate(t.Context(), turn("1"))
	require.Equal(t, reason, "")
	_, second, _ := l.translate(t.Context(), turn("2"))
	assert.Equal(t, string(second), string(first))
	var req struct {
		Messages []oaMsg `json:"messages"`
		Tools    []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(first, &req))
	assert.Equal(t, req.Messages[0].Content, "You are a coding agent.")
	require.Equal(t, len(req.Tools), 2)
	assert.Equal(t, req.Tools[0].Function.Name, "weather", "tools keep the agent's order")
	assert.Equal(t, string(req.Tools[1].Function.Parameters), `{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}`, "schemas are sent as written")
}

// slowServer waits before its first chunk and between the first and the rest,
// or until the gateway hangs up.
func slowServer(t *testing.T, beforeFirst, afterFirst time.Duration) *httptest.Server {
	t.Helper()
	pause := func(r *http.Request, d time.Duration) bool {
		select {
		case <-time.After(d):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if !pause(r, beforeFirst) {
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"o"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		if !pause(r, afterFirst) {
			return
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"k"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
}

func TestLocalTimeouts(t *testing.T) {
	const haiku = `{"model":"claude-haiku-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	const sonnet = `{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	cases := map[string]struct {
		before, after       time.Duration
		firstToken, timeout time.Duration
		body                string
		ok                  bool
	}{
		"slow prefill falls through":          {before: time.Second, firstToken: 50 * time.Millisecond, body: haiku},
		"slow prefill of a held answer":       {before: time.Second, firstToken: 50 * time.Millisecond, body: sonnet},
		"started in time may take longer":     {before: 10 * time.Millisecond, after: 150 * time.Millisecond, firstToken: 50 * time.Millisecond, body: sonnet, ok: true},
		"held answer over the whole bound":    {after: time.Second, firstToken: time.Second, timeout: 100 * time.Millisecond, body: sonnet},
		"no bounds waits for the model":       {before: 50 * time.Millisecond, body: haiku, ok: true},
		"first token bound alone is no limit": {after: 150 * time.Millisecond, firstToken: 50 * time.Millisecond, body: sonnet, ok: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := slowServer(t, c.before, c.after)
			defer srv.Close()
			l := &Local{BaseURL: srv.URL, Model: "q", FirstToken: c.firstToken, Timeout: c.timeout}
			rec := httptest.NewRecorder()
			_, reason, ok := l.serve(t.Context(), rec, []byte(c.body))
			assert.Equal(t, ok, c.ok)
			if c.ok {
				assert.Equal(t, reason, "")
				return
			}
			assert.Equal(t, reason, FallTimeout)
			assert.Equal(t, rec.Body.Len(), 0, "nothing is written, so the cloud can answer")
		})
	}
}

func TestLocalCancelIsNotTimeout(t *testing.T) {
	srv := slowServer(t, time.Second, 0)
	defer srv.Close()
	l := &Local{BaseURL: srv.URL, Model: "q", FirstToken: 5 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	defer time.AfterFunc(50*time.Millisecond, cancel).Stop()
	_, reason, ok := l.serve(ctx, httptest.NewRecorder(), []byte(`{"model":"claude-haiku-5-5","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	assert.True(t, !ok)
	assert.Equal(t, reason, FallCancelled, "the agent leaving is not the gateway's timeout")
}
