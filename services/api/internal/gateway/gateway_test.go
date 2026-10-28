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
		"main model":   {`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`, 200, nil, FallNotEligible},
		"server tool":  {`{"model":"claude-haiku-5-5","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"hi"}]}`, 200, nil, FallNotEligible},
		"has image":    {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":[{"type":"image"}]}]}`, 200, nil, FallNotEligible},
		"local errors": {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`, 500, nil, FallUnavailable},
		"queue timeout": {`{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`, 200,
			func(ctx context.Context) (func(), error) { <-ctx.Done(); return nil, ctx.Err() }, FallBusy},
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
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Body.String(), "cloud")
			assert.Equal(t, rec.Header().Get(RouteHeader), "cloud")
			assert.Equal(t, meta.Fallthrough, tc.reason, "why the local model did not answer")
		})
	}
}

func TestReportedUsage(t *testing.T) {
	in, out := reportedUsage([]byte(`data: {"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	assert.Equal(t, []int{in, out}, []int{12, 34})
	in, out = reportedUsage([]byte("data: [DONE]"))
	assert.Equal(t, []int{in, out}, []int{0, 0})
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
