package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestPromptMeta(t *testing.T) {
	agentLoop := `{"model":"claude-opus-5-5","max_tokens":100,"stream":%s,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"fix the bug"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"SECRET FILE"}]}]}`
	plain := `{"model":"claude-opus-5-5","max_tokens":100,"stream":%s,"messages":[{"role":"user","content":"hi there"}]}`
	cases := map[string]struct {
		body    string
		stream  string
		capture bool
		local   bool
		prompt  string
		answer  string
	}{
		"local stream, capturing":   {plain, "true", true, true, "hi there", "Hello"},
		"local json, capturing":     {plain, "false", true, true, "hi there", "Hello"},
		"local, not capturing":      {plain, "true", false, true, "hi there", ""},
		"cloud keeps no answer":     {plain, "true", true, false, "hi there", ""},
		"tool results are left out": {agentLoop, "false", true, false, "fix the bug", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, completeAnswer) }))
			defer loc.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cloud") }))
			defer cloud.Close()
			g := NewAnthropic()
			g.Upstream = cloud.URL
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen"}
			g.Local = func() *Local { return l }
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			rec := httptest.NewRecorder()
			body := strings.Replace(tc.body, "%s", tc.stream, 1)
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r = r.WithContext(WithPromptCapture(WithLocalFirst(r.Context(), tc.local), tc.capture))
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)

			assert.Equal(t, meta.PromptSHA256, digestOf(tc.prompt), "the digest is always taken")
			assert.Equal(t, meta.PromptBytes, len(tc.prompt))
			if tc.capture {
				assert.Equal(t, meta.Prompt, tc.prompt)
			} else {
				assert.Equal(t, meta.Prompt, "", "no text unless the organisation keeps a prompt log")
			}
			assert.Equal(t, meta.Answer, tc.answer)
			assert.False(t, meta.TextChanged)
			assert.NotContains(t, meta.Prompt, "SECRET")
		})
	}
}

func TestNoteForwarded(t *testing.T) {
	sent := []byte(`{"messages":[{"role":"user","content":"please could you kindly fix the bug"}]}`)
	short := []byte(`{"messages":[{"role":"user","content":"fix the bug"}]}`)
	for name, tc := range map[string]struct {
		capture bool
		fwd     []byte
		changed bool
	}{
		"unchanged":          {true, sent, false},
		"shortened":          {true, short, true},
		"shortened, no text": {false, short, true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := WithPromptCapture(context.Background(), tc.capture)
			var m Meta
			m.notePrompt(ctx, anthropicUserText(sent))
			m.noteForwarded(ctx, sent, tc.fwd)
			assert.Equal(t, m.TextChanged, tc.changed)
			if !tc.changed {
				assert.Equal(t, m.ForwardedSHA256, "")
				return
			}
			assert.Equal(t, m.ForwardedSHA256, digestOf("fix the bug"))
			assert.Equal(t, m.ForwardedBytes, len("fix the bug"))
			assert.Equal(t, m.Forwarded != "", tc.capture)
		})
	}
}

func TestOpenAIPromptMeta(t *testing.T) {
	for name, tc := range map[string]struct {
		stream bool
		answer string
	}{
		"stream": {true, completeAnswer},
		"json":   {false, `{"choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`},
	} {
		t.Run(name, func(t *testing.T) {
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.answer) }))
			defer loc.Close()
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen"}
			var meta Meta
			o := &OpenAI{Local: func() *Local { return l }, Done: func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }}
			body := `{"model":"x","messages":[{"role":"system","content":"be terse"},{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			r = r.WithContext(WithPromptCapture(r.Context(), true))
			o.ChatHandler()(httptest.NewRecorder(), r)
			assert.Equal(t, meta.Route, "local")
			assert.Equal(t, meta.Prompt, "hello", "the system prompt is never part of it")
			assert.Equal(t, meta.PromptSHA256, digestOf("hello"))
			assert.Equal(t, meta.Answer, "Hello")
		})
	}
}

func FuzzAnswerText(f *testing.F) {
	f.Add([]byte(completeAnswer))
	f.Add([]byte(`{"content":[{"type":"text","text":"x"}]}`))
	f.Add([]byte("event: x\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"y\"}}\n"))
	f.Fuzz(func(_ *testing.T, raw []byte) {
		_ = anthropicAnswerText(raw)
		_ = openAIAnswerText(raw)
		_ = anthropicUserText(raw)
	})
}
