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

const (
	completeAnswer = "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	emptyAnswer     = "data: {\"choices\":[{\"delta\":{\"content\":\"  \"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	truncatedAnswer = "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
	brokenOffAnswer = "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"
)

func TestLocalFirst(t *testing.T) {
	opus := `{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	cases := map[string]struct {
		first  bool
		window int
		answer string
		body   string
		route  string
		reason string
	}{
		"main model answered locally": {true, 0, completeAnswer, opus, "local", ""},
		"switched off":                {false, 0, completeAnswer, opus, "cloud", FallNotEligible},
		"larger than the window":      {true, 50, completeAnswer, opus, "cloud", FallTooLarge},
		"fits the window":             {true, 8192, completeAnswer, opus, "local", ""},
		"empty answer":                {true, 0, emptyAnswer, opus, "cloud", FallEmptyAnswer},
		"stopped at max tokens":       {true, 0, truncatedAnswer, opus, "cloud", FallTruncated},
		"stream broke off":            {true, 0, brokenOffAnswer, opus, "cloud", FallTruncated},
		"nothing came":                {true, 0, "data: [DONE]\n\n", opus, "cloud", FallEmptyAnswer},
		"large request, known window": {true, 1 << 20, completeAnswer, `{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"` + strings.Repeat("a", 512<<10) + `"}]}`, "local", ""},
		"large request, no window":    {true, 0, completeAnswer, `{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"` + strings.Repeat("a", 512<<10) + `"}]}`, "cloud", FallTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.answer) }))
			defer loc.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cloud") }))
			defer cloud.Close()
			g := NewAnthropic()
			g.Upstream = cloud.URL
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", ContextWindow: tc.window}
			g.Local = func() *Local { return l }
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(tc.body))
			r = r.WithContext(WithLocalFirst(r.Context(), tc.first))
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Header().Get(RouteHeader), tc.route)
			assert.Equal(t, meta.Fallthrough, tc.reason)
			if tc.route == "cloud" {
				assert.Equal(t, rec.Body.String(), "cloud")
				return
			}
			assert.Equal(t, rec.Code, 200)
			assert.Contains(t, rec.Body.String(), `"stop_reason":"end_turn"`)
			assert.Contains(t, rec.Body.String(), "Hel")
		})
	}
}

// TestLocalExclude covers the routing decision itself: every model goes to
// the local model by default, and an exclude list (the installation's or the
// organisation's) sends matching models to the cloud, local first or not.
func TestLocalExclude(t *testing.T) {
	cases := map[string]struct {
		first   *bool // nil: the request carries no choice
		install []string
		org     []string
		model   string
		route   string
		reason  string
	}{
		"no choice means local first":         {nil, nil, nil, "claude-opus-5-5", "local", ""},
		"every model by default":              {ptr(true), nil, nil, "gpt-5", "local", ""},
		"excluded exact":                      {ptr(true), nil, []string{"claude-opus-5-5"}, "claude-opus-5-5", "cloud", FallExcluded},
		"exact does not match a longer name":  {ptr(true), nil, []string{"claude-opus-5"}, "claude-opus-5-5", "local", ""},
		"excluded prefix":                     {ptr(true), nil, []string{"claude-opus-*"}, "Claude-Opus-4-1", "cloud", FallExcluded},
		"prefix leaves others":                {ptr(true), nil, []string{"claude-opus-*"}, "claude-sonnet-5-5", "local", ""},
		"installation list":                   {ptr(true), []string{"gpt-5"}, nil, "gpt-5", "cloud", FallExcluded},
		"both lists add up":                   {ptr(true), []string{"gpt-5"}, []string{"o3"}, "o3", "cloud", FallExcluded},
		"local first off, small model":        {ptr(false), nil, nil, "claude-haiku-5-5", "local", ""},
		"local first off, main model":         {ptr(false), nil, nil, "claude-opus-5-5", "cloud", FallNotEligible},
		"exclusion wins with local first off": {ptr(false), nil, []string{"claude-haiku-*"}, "claude-haiku-5-5", "cloud", FallExcluded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, completeAnswer) }))
			defer loc.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "cloud") }))
			defer cloud.Close()
			g := NewAnthropic()
			g.Upstream = cloud.URL
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", Exclude: tc.install}
			g.Local = func() *Local { return l }
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+tc.model+`","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
			ctx := WithExclude(r.Context(), tc.org)
			if tc.first != nil {
				ctx = WithLocalFirst(ctx, *tc.first)
			}
			r = r.WithContext(ctx)
			r.Header.Set("X-Api-Key", "k")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Header().Get(RouteHeader), tc.route)
			assert.Equal(t, meta.Fallthrough, tc.reason)
		})
	}
}

func ptr(b bool) *bool { return &b }
