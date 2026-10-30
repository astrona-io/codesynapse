package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestForwardPassesCredentialAndStreams(t *testing.T) {
	var got http.Header
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		assert.Equal(t, r.URL.Path, "/v1/messages")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "secret=1")
		_, _ = io.WriteString(w, "event: ping\n\n")
	}))
	defer up.Close()

	var meta Meta
	g := NewAnthropic()
	g.Upstream = up.URL
	g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
	srv := httptest.NewServer(g.Handler("/v1/messages"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"model":"m1","stream":true,"tools":[{}]}`))
	req.Header.Set("X-Api-Key", "sk-ant-x")
	req.Header.Set(KeyHeader, "csk_secret")
	req.Header.Set("Cookie", "cs_session=abc")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // read side
	out, _ := io.ReadAll(resp.Body)

	assert.Equal(t, string(out), "event: ping\n\n")
	assert.Equal(t, resp.Header.Get("Set-Cookie"), "")
	assert.Equal(t, got.Get("X-Api-Key"), "sk-ant-x")
	assert.Equal(t, got.Get("Anthropic-Version"), "2023-06-01")
	assert.Equal(t, got.Get(KeyHeader), "")
	assert.Equal(t, got.Get("Cookie"), "")
	assert.Equal(t, gotBody, `{"model":"m1","stream":true,"tools":[{}]}`)
	assert.True(t, meta.FirstByte > 0, "the first byte of the stream is timed")
	meta.FirstByte = 0
	assert.Equal(t, meta, Meta{Path: "/v1/messages", Model: "m1", Stream: true, Bytes: len(gotBody), Tools: true, Route: "cloud", Out: 13})
}

func TestNoCredentialIsRefused(t *testing.T) {
	g := NewAnthropic()
	rec := httptest.NewRecorder()
	g.Handler("/v1/messages")(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	assert.Equal(t, rec.Code, http.StatusUnauthorized)
}

func TestRedirectIsNotFollowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
	}))
	defer up.Close()
	g := NewAnthropic()
	g.Upstream = up.URL
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	r.Header.Set("X-Api-Key", "k")
	g.Handler("/v1/messages")(rec, r)
	assert.Equal(t, rec.Code, http.StatusFound)
}

func TestGatewayKeyIsNeverForwardedAsModelCredential(t *testing.T) {
	hit := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer up.Close()
	g := NewAnthropic()
	g.Upstream = up.URL
	for name, set := range map[string]func(*http.Request){
		"bearer":    func(r *http.Request) { r.Header.Set("Authorization", "Bearer csk_abc") },
		"x-api-key": func(r *http.Request) { r.Header.Set("X-Api-Key", "csk_abc") },
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			set(r)
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Code, http.StatusBadRequest)
			assert.False(t, hit, "nothing reaches the upstream")
		})
	}
}

func TestLocalRedirectIsNotFollowed(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer loc.Close()
	o := &OpenAI{Local: func() *Local { return &Local{BaseURL: loc.URL + "/v1", Model: "m", APIKey: "secret"} }}
	rec := httptest.NewRecorder()
	o.ChatHandler()(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
	assert.False(t, followed, "the local model's bearer key must not follow a redirect")
}

func TestBodyIsCapped(t *testing.T) {
	g := NewAnthropic()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(zeros{}, MaxBody+1))
	r.Header.Set("X-Api-Key", "k")
	g.Handler("/v1/messages")(rec, r)
	assert.Equal(t, rec.Code, http.StatusRequestEntityTooLarge)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func localServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/v1/chat/completions")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"+
			"data: [DONE]\n\n")
	}))
}

func TestSmallRequestIsAnsweredLocally(t *testing.T) {
	loc := localServer(t, http.StatusOK)
	defer loc.Close()
	cloudHit := false
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cloudHit = true }))
	defer cloud.Close()
	g := NewAnthropic()
	g.Upstream = cloud.URL
	released := false
	l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", Acquire: func(context.Context) (func(), error) {
		return func() { released = true }, nil
	}}
	g.Local = func() *Local { return l }
	var meta Meta
	g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
	body := `{"model":"claude-haiku-5-5","max_tokens":50,"system":"be brief","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// No Anthropic credential is needed for a local answer.
	rec := httptest.NewRecorder()
	g.Handler("/v1/messages")(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	assert.Equal(t, rec.Code, 200)
	assert.Equal(t, rec.Header().Get(RouteHeader), "local")
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", `"text":"Hel"`, `"text":"lo"`, `"stop_reason":"end_turn"`, `"output_tokens":2`, "event: message_stop"} {
		assert.True(t, strings.Contains(out, want), want)
	}
	assert.True(t, !cloudHit)
	assert.True(t, released)
	assert.Equal(t, meta.Route, "local")
	assert.True(t, meta.Metered)
	assert.Equal(t, []int{meta.InTokens, meta.OutTokens}, []int{7, 2})
	assert.Equal(t, meta.Out, rec.Body.Len(), "response bytes are counted")

	rec = httptest.NewRecorder()
	g.Handler("/v1/messages")(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Replace(body, `"stream":true`, `"stream":false`, 1))))
	assert.True(t, strings.Contains(rec.Body.String(), `"text":"Hello"`))
}

func TestLocalFallsThroughToCloud(t *testing.T) {
	cases := map[string]struct {
		body   string
		status int
		local  func(context.Context) (func(), error)
		reason string
	}{
		"main model, local first off": {`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`, 200, nil, FallNotEligible},
		"server tool":                 {`{"model":"claude-haiku-5-5","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`, 200, nil, FallUnsupportedTool},
		"has image":                   {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"image"}]}]}`, 200, nil, FallHasImage},
		"has document":                {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"document"}]}]}`, 200, nil, FallHasImage},
		"image in a tool result":      {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image"}]}]}]}`, 200, nil, FallHasImage},
		"thinking asked":              {`{"model":"claude-haiku-5-5","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`, 200, nil, FallHasThinking},
		"thinking sent back":          {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}]}`, 200, nil, FallHasThinking},
		"local errors":                {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`, 500, nil, FallBadStatus},
		"queue timeout": {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`, 200,
			func(ctx context.Context) (func(), error) { <-ctx.Done(); return nil, ctx.Err() }, FallQueueTimeout},
		"queue broke": {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`, 200,
			func(context.Context) (func(), error) { return nil, errors.New("closed") }, FallModelError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			loc := localServer(t, tc.status)
			defer loc.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cloud") }))
			defer cloud.Close()
			g := NewAnthropic()
			g.Upstream = cloud.URL
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", Acquire: tc.local, Wait: 20 * time.Millisecond}
			g.Local = func() *Local { return l }
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			r = r.WithContext(WithLocalThinking(WithLocalFirst(r.Context(), false), false))
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Body.String(), "cloud")
			assert.Equal(t, rec.Header().Get(RouteHeader), "cloud")
			assert.Equal(t, meta.Fallthrough, tc.reason, "why the local model did not answer")
			if tc.reason == FallQueueTimeout {
				assert.True(t, meta.Asked, "it reached the queue")
				assert.True(t, meta.QueueWait >= 20*time.Millisecond, "the wait is measured")
			}
		})
	}
}

func TestLocalFailuresAreTold(t *testing.T) {
	body := `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	cases := map[string]struct {
		local  *Local
		reason string
	}{
		"answer timeout": {&Local{BaseURL: slow.URL, Client: &http.Client{Timeout: 30 * time.Millisecond}}, FallTimeout},
		"not reachable":  {&Local{BaseURL: gone.URL}, FallModelError},
		"no local model": {nil, FallNoLocalModel},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cloud") }))
			defer cloud.Close()
			g := NewAnthropic()
			g.Upstream = cloud.URL
			g.Local = func() *Local { return tc.local }
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Body.String(), "cloud")
			assert.Equal(t, meta.Route, "cloud")
			assert.Equal(t, meta.Fallthrough, tc.reason)
			assert.True(t, meta.FirstByte > 0, "the first byte is timed")
			if tc.local != nil {
				assert.True(t, meta.Asked && meta.LocalAnswer > 0, "the local model's time is measured")
			}
		})
	}
}

func TestFailureNamesTimeouts(t *testing.T) {
	live := context.Background()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx  context.Context
		err  error
		want string
	}{
		"deadline":        {live, context.DeadlineExceeded, FallTimeout},
		"wrapped":         {live, fmt.Errorf("read: %w", context.DeadlineExceeded), FallTimeout},
		"net timeout":     {live, &net.DNSError{IsTimeout: true}, FallTimeout},
		"refused":         {live, errors.New("connection refused"), FallModelError},
		"agent went away": {gone, context.Canceled, FallCancelled},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, failure(tc.ctx, tc.err), tc.want) })
	}
}

func TestReportedUsage(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		want       []int
	}{
		{"no cache reported (Ollama)", `data: {"usage":{"prompt_tokens":12,"completion_tokens":34}}`, []int{12, 34, 0}},
		{"cached_tokens (vLLM, LM Studio)", `data: {"usage":{"prompt_tokens":120,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":96}}}`, []int{120, 4, 96}},
		{"cache_n (llama.cpp)", `data: {"usage":{"prompt_tokens":50,"completion_tokens":5},"timings":{"cache_n":40,"prompt_n":10}}`, []int{50, 5, 40}},
		{"cached_tokens wins over cache_n", `data: {"usage":{"prompt_tokens":50,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":30}},"timings":{"cache_n":40}}`, []int{50, 5, 30}},
		{"never more than the prompt", `data: {"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":99}}}`, []int{10, 1, 10}},
		{"nothing", "data: [DONE]", []int{0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, out, cached := reportedUsage([]byte(tc.tail))
			assert.Equal(t, []int{in, out, cached}, tc.want)
		})
	}
}

func TestReadChunkCache(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		cached     int
	}{
		{"none reported (Ollama)", `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1}}`, 0},
		{"cached_tokens", `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":6}}}`, 6},
		{"llama.cpp cache_n", `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1},"timings":{"cache_n":7}}`, 7},
		{"clamped to the prompt", `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":1},"timings":{"cache_n":70}}`, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := readChunk(bufio.NewScanner(strings.NewReader(tc.line + "\n")))
			require.True(t, ok)
			assert.Equal(t, c.Cached, tc.cached)
		})
	}
}

func TestOpenAIChatQueuesAndUsesLocalModel(t *testing.T) {
	var got string
	loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer local-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n")
	}))
	defer loc.Close()
	waited := false
	l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", APIKey: "local-key", Acquire: func(context.Context) (func(), error) {
		waited = true
		return func() {}, nil
	}}
	o := &OpenAI{Local: func() *Local { return l }}
	rec := httptest.NewRecorder()
	o.ChatHandler()(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-x","stream":true,"messages":[],"tools":[{"a":1}]}`)))
	assert.Equal(t, rec.Code, 200)
	assert.True(t, waited)
	assert.True(t, strings.Contains(got, `"model":"qwen"`))
	assert.True(t, strings.Contains(got, `"tools":[{"a":1}]`), "other fields pass through")
	assert.Equal(t, rec.Header().Get(RouteHeader), "local")

	rec = httptest.NewRecorder()
	o.ModelsHandler()(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	assert.True(t, strings.Contains(rec.Body.String(), `"id":"qwen"`))

	off := &OpenAI{}
	rec = httptest.NewRecorder()
	off.ChatHandler()(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	assert.Equal(t, rec.Code, http.StatusServiceUnavailable)
	rec = httptest.NewRecorder()
	o.ChatHandler()(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("[")))
	assert.Equal(t, rec.Code, http.StatusBadRequest)
}
