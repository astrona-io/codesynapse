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
		"empty answer":                {true, 0, emptyAnswer, opus, "cloud", FallBadAnswer},
		"stopped at max tokens":       {true, 0, truncatedAnswer, opus, "cloud", FallBadAnswer},
		"stream broke off":            {true, 0, brokenOffAnswer, opus, "cloud", FallBadAnswer},
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
