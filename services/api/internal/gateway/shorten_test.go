package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// prose is a typed prompt long enough to be shortened.
var prose = strings.Repeat("Please could you take a careful look at the parser in the ingest package and tell me why it fails. ", 6)

func sessionCtx(t *testing.T, session string) context.Context {
	t.Helper()
	ctx, err := execution.With(context.Background(), execution.Identity{TenantID: "org-1", SubjectID: "user-1", SessionID: session, ParentSessionID: session})
	require.NoError(t, err)
	return ctx
}

// fakeShortener halves every text it is asked about and counts the calls.
func fakeShortener(calls *atomic.Int32) *Shortener {
	return &Shortener{
		Ready: func() bool { return true },
		Rewrite: func(_ context.Context, text string) (string, bool) {
			calls.Add(1)
			return "short: " + text[:len(text)/2], true
		},
	}
}

type msg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func body(t *testing.T, msgs ...msg) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "claude-x", "system": prose, "messages": msgs})
	require.NoError(t, err)
	return b
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func TestShortenReplaysIdenticalBytesAcrossTurns(t *testing.T) {
	var calls atomic.Int32
	s := fakeShortener(&calls)
	ctx := sessionCtx(t, "sess-1")

	turn1 := body(t, msg{"user", prose})
	out1, res := s.Apply(ctx, turn1)
	require.True(t, res.Changed)
	assert.Equal(t, calls.Load(), int32(1))
	assert.True(t, res.New > 0)
	assert.Equal(t, res.New, res.Removed)
	assert.Equal(t, len(turn1)-len(out1), res.Removed)

	second := strings.Replace(prose, "parser", "lexer", 1)
	turn2 := body(t, msg{"user", prose}, msg{"assistant", "It fails on tabs."}, msg{"user", []any{textBlock(second)}})
	out2, res2 := s.Apply(ctx, turn2)
	assert.Equal(t, calls.Load(), int32(2), "one call for the new message")
	assert.Equal(t, res2.New, len(second)-len("short: "+second[:len(second)/2]))
	assert.True(t, res2.Removed > res2.New, "the replay counts as removed, not as new")

	// The first message goes out exactly as it did on turn one.
	var m1, m2 struct {
		Messages []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out1, &m1))
	require.NoError(t, json.Unmarshal(out2, &m2))
	assert.Equal(t, string(m2.Messages[0]), string(m1.Messages[0]))

	// A third turn replays both without calling the model again.
	turn3 := body(t, msg{"user", prose}, msg{"assistant", "It fails on tabs."}, msg{"user", []any{textBlock(second)}}, msg{"assistant", "ok"}, msg{"user", "thanks"})
	out3, res3 := s.Apply(ctx, turn3)
	assert.Equal(t, calls.Load(), int32(2))
	assert.Equal(t, res3.New, 0)
	var m3 struct {
		Messages []json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out3, &m3))
	for i, m := range m2.Messages {
		assert.Equal(t, string(m3.Messages[i]), string(m), "the earlier turns keep their bytes")
	}

	// Another session has its own memo and never replays this one's.
	_, res4 := s.Apply(sessionCtx(t, "sess-2"), turn3)
	assert.Equal(t, res4, Shortened{})
}

func TestShortenLeavesOtherBlocksAlone(t *testing.T) {
	reminder := "<system-reminder>\n" + prose + "\n</system-reminder>"
	code := prose + "\n```go\nfunc main() {}\n```\n"
	toolUse := map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"text": prose}}
	toolResult := map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": prose}
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGk="}, "text": prose}
	cases := []struct {
		name string
		msgs []msg
	}{
		{"tool result", []msg{{"user", []any{toolResult}}}},
		{"tool use", []msg{{"assistant", []any{toolUse}}, {"user", []any{toolResult}}}},
		{"image", []msg{{"user", []any{image}}}},
		{"reminder", []msg{{"user", []any{textBlock(reminder)}}}},
		{"reminder later in the text", []msg{{"user", prose + "<system-reminder>x</system-reminder>"}}},
		{"code fence", []msg{{"user", code}}},
		{"short", []msg{{"user", "fix the build please"}}},
		{"assistant text", []msg{{"assistant", prose}}},
		{"older user message", []msg{{"user", prose}, {"assistant", "ok"}}},
		{"too long", []msg{{"user", strings.Repeat(prose, 40)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			in := body(t, tc.msgs...)
			out, res := fakeShortener(&calls).Apply(sessionCtx(t, "sess"), in)
			assert.Equal(t, string(out), string(in))
			assert.Equal(t, res, Shortened{})
			assert.Equal(t, calls.Load(), int32(0))
		})
	}
}

func TestShortenKeepsOriginalOnDoubt(t *testing.T) {
	in := body(t, msg{"user", prose})
	cases := []struct {
		name    string
		ctx     func(t *testing.T) context.Context
		s       *Shortener
		badBody []byte
	}{
		{"rewrite refused (fact check)", nil, &Shortener{Rewrite: func(context.Context, string) (string, bool) { return "", false }}, nil},
		{"rewrite not shorter", nil, &Shortener{Rewrite: func(_ context.Context, s string) (string, bool) { return s + "!", true }}, nil},
		{"rewrite times out", nil, &Shortener{Rewrite: func(ctx context.Context, _ string) (string, bool) {
			<-ctx.Done()
			return "", false
		}}, nil},
		{"no local model", nil, &Shortener{Ready: func() bool { return false }, Rewrite: func(context.Context, string) (string, bool) { return "x", true }}, nil},
		{"no session", func(*testing.T) context.Context { return context.Background() }, &Shortener{Rewrite: func(context.Context, string) (string, bool) { return "x", true }}, nil},
		{"duplicate key", nil, &Shortener{Rewrite: func(context.Context, string) (string, bool) { return "x", true }},
			[]byte(`{"messages":[{"role":"user","content":"` + prose + `"}],"messages":[]}`)},
		{"duplicate role", nil, &Shortener{Rewrite: func(context.Context, string) (string, bool) { return "x", true }},
			[]byte(`{"messages":[{"role":"assistant","role":"user","content":"` + prose + `"}]}`)},
		{"invalid json", nil, &Shortener{Rewrite: func(context.Context, string) (string, bool) { return "x", true }},
			[]byte(`{"messages":[{"role":"user","content":"` + prose + `"}]`)},
		{"nil shortener", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := sessionCtx(t, "sess")
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}
			if tc.name == "rewrite times out" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			b := in
			if tc.badBody != nil {
				b = tc.badBody
			}
			out, res := tc.s.Apply(ctx, b)
			assert.Equal(t, string(out), string(b))
			assert.Equal(t, res, Shortened{})
		})
	}
}

func TestShortenRefusalIsRemembered(t *testing.T) {
	var calls atomic.Int32
	s := &Shortener{Rewrite: func(context.Context, string) (string, bool) { calls.Add(1); return "", false }}
	ctx := sessionCtx(t, "sess")
	in := body(t, msg{"user", prose})
	s.Apply(ctx, in)
	s.Apply(ctx, in)
	assert.Equal(t, calls.Load(), int32(1), "a refused block is not asked about again")
}

func TestShortenOneCallPerRequest(t *testing.T) {
	var calls atomic.Int32
	s := fakeShortener(&calls)
	other := strings.Replace(prose, "parser", "lexer", 1)
	_, res := s.Apply(sessionCtx(t, "sess"), body(t, msg{"user", []any{textBlock(prose), textBlock(other)}}))
	assert.Equal(t, calls.Load(), int32(1))
	assert.True(t, res.Changed)
}

func TestShortenMemoExpires(t *testing.T) {
	var calls atomic.Int32
	s := fakeShortener(&calls)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := sessionCtx(t, "sess")
	in := body(t, msg{"user", prose})
	s.Apply(ctx, in)
	now = now.Add(memoTTL + time.Minute)
	s.Apply(ctx, in)
	assert.Equal(t, calls.Load(), int32(2), "an expired entry is made again")
}

func TestShortenMemoIsBounded(t *testing.T) {
	var calls atomic.Int32
	s := fakeShortener(&calls)
	ctx := sessionCtx(t, "sess")
	for i := range memoPerSession + 10 {
		s.Apply(ctx, body(t, msg{"user", prose + strings.Repeat("x", i)}))
	}
	s.memo.mu.Lock()
	n := s.memo.sessions["sess"].lru.Len()
	s.memo.mu.Unlock()
	assert.Equal(t, n, memoPerSession)
	for i := range memoSessions + 5 {
		s.Apply(sessionCtx(t, "s-"+strings.Repeat("a", i%50)+string(rune('a'+i%26))+strings.Repeat("b", i/26)), body(t, msg{"user", prose}))
	}
	s.memo.mu.Lock()
	assert.True(t, len(s.memo.sessions) <= memoSessions)
	s.memo.mu.Unlock()
}

func TestHandlerShortensUnlessTurnedOff(t *testing.T) {
	in := string(body(t, msg{"user", prose}))
	for _, off := range []bool{false, true} {
		t.Run(map[bool]string{false: "on by default", true: "turned off"}[off], func(t *testing.T) {
			var got string
			up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				got = string(b)
			}))
			defer up.Close()
			var calls atomic.Int32
			var meta Meta
			g := NewAnthropic()
			g.Upstream = up.URL
			g.Shorten = fakeShortener(&calls)
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			ctx := sessionCtx(t, "sess")
			if off {
				ctx = WithShortenPrompts(ctx, false)
			}
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(in))
			r.Header.Set("X-Api-Key", "sk-ant-x")
			g.Handler("/v1/messages")(httptest.NewRecorder(), r)
			if off {
				assert.Equal(t, got, in)
				assert.False(t, meta.Shortened)
				assert.Equal(t, calls.Load(), int32(0))
				return
			}
			assert.True(t, meta.Shortened)
			assert.Equal(t, len(in)-len(got), meta.Removed)
			assert.Equal(t, meta.Saved, meta.Removed)
			assert.Equal(t, meta.Bytes, len(in), "the size the agent sent")
			assert.True(t, json.Valid([]byte(got)))
		})
	}
}

// gatewayBody is a request for model with the typed prose as its only text,
// plus extra top-level fields; content replaces the user's content when set.
func gatewayBody(t *testing.T, model string, content any, extra map[string]any) string {
	t.Helper()
	if content == nil {
		content = prose
	}
	v := map[string]any{"model": model, "max_tokens": 100, "system": "Be brief.", "messages": []msg{{"user", content}}}
	for k, x := range extra {
		v[k] = x
	}
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// TestFallthroughIsShortened covers every reason the local model can leave a
// request to the cloud: the local model is offered the text as typed, and
// the cloud leg that follows gets the shortened text. FallCancelled is left
// out: the agent is gone, so there is no cloud leg either.
func TestFallthroughIsShortened(t *testing.T) {
	const opus, haiku = "claude-opus-5-5", "claude-haiku-5-5"
	badTool := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"nope","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	weather := []any{map[string]any{"name": "weather", "input_schema": map[string]any{"type": "object"}}}
	cases := map[string]struct {
		model   string
		content any
		extra   map[string]any
		first   *bool
		window  int
		status  int
		answer  string
		slow    bool
		acquire func(context.Context) (func(), error)
		none    bool // no local model
		reason  string
	}{
		"not eligible":   {model: opus, first: ptr(false), reason: FallNotEligible},
		"too large":      {model: opus, window: 50, reason: FallTooLarge},
		"has thinking":   {model: haiku, extra: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}, reason: FallHasThinking},
		"has image":      {model: haiku, content: []any{textBlock(prose), map[string]any{"type": "image"}}, reason: FallHasImage},
		"queue timeout":  {model: haiku, acquire: func(ctx context.Context) (func(), error) { <-ctx.Done(); return nil, ctx.Err() }, reason: FallQueueTimeout},
		"model error":    {model: haiku, acquire: func(context.Context) (func(), error) { return nil, errors.New("closed") }, reason: FallModelError},
		"answer timeout": {model: haiku, slow: true, reason: FallTimeout},
		"bad status":     {model: haiku, status: http.StatusInternalServerError, reason: FallBadStatus},
		"empty answer":   {model: opus, answer: emptyAnswer, reason: FallEmptyAnswer},
		"truncated":      {model: opus, answer: truncatedAnswer, reason: FallTruncated},
		"bad answer":     {model: haiku, extra: map[string]any{"tools": weather}, answer: badTool, reason: FallBadAnswer},
		"no local model": {model: haiku, none: true, reason: FallNoLocalModel},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := gatewayBody(t, tc.model, tc.content, tc.extra)
			var mu sync.Mutex
			var localGot string
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				localGot = string(b)
				mu.Unlock()
				if tc.slow {
					select {
					case <-r.Context().Done():
					case <-time.After(2 * time.Second):
					}
					return
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = io.WriteString(w, tc.answer)
			}))
			defer loc.Close()
			var cloudGot string
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				cloudGot = string(b)
				_, _ = io.WriteString(w, "cloud")
			}))
			defer cloud.Close()
			var calls atomic.Int32
			g := NewAnthropic()
			g.Upstream = cloud.URL
			g.Shorten = fakeShortener(&calls)
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", ContextWindow: tc.window, Acquire: tc.acquire, Wait: 20 * time.Millisecond}
			if tc.slow {
				l.Client = &http.Client{Timeout: 30 * time.Millisecond}
			}
			g.Local = func() *Local {
				if tc.none {
					return nil
				}
				return l
			}
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			// Thinking requests reach the cloud only with local thinking off.
			ctx := WithLocalThinking(sessionCtx(t, "sess"), false)
			if tc.first != nil {
				ctx = WithLocalFirst(ctx, *tc.first)
			}
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(in))
			r.Header.Set("X-Api-Key", "sk-ant-x")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Body.String(), "cloud")
			assert.Equal(t, meta.Fallthrough, tc.reason)
			assert.True(t, meta.Shortened, "the cloud leg is shortened")
			assert.True(t, meta.Removed > 0)
			assert.Equal(t, len(in)-len(cloudGot), meta.Removed)
			assert.Contains(t, cloudGot, "short: ")
			assert.True(t, json.Valid([]byte(cloudGot)))
			mu.Lock()
			defer mu.Unlock()
			if localGot != "" {
				assert.Contains(t, localGot, strings.TrimSpace(prose), "the local model gets the text as typed")
				assert.False(t, strings.Contains(localGot, "short: "))
			}
		})
	}
}

// TestExcludedModelIsNotShortened: a model on an exclude list goes to the
// cloud as the agent sent it, whatever else would have kept it from the
// local model.
func TestExcludedModelIsNotShortened(t *testing.T) {
	cases := map[string]struct {
		install, org []string
		large        bool
		none         bool
		reason       string
	}{
		"organisation list":           {org: []string{"claude-opus-*"}, reason: FallExcluded},
		"installation list":           {install: []string{"claude-opus-5-5"}, reason: FallExcluded},
		"too large and excluded":      {org: []string{"claude-opus-*"}, large: true, reason: FallTooLarge},
		"no local model and excluded": {org: []string{"claude-opus-5-5"}, none: true, reason: FallNoLocalModel},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var extra map[string]any
			if tc.large {
				// Over the local bound, so the request is refused before the
				// exclude lists are read.
				extra = map[string]any{"metadata": map[string]any{"pad": strings.Repeat("a", maxLocalBody)}}
			}
			in := gatewayBody(t, "claude-opus-5-5", nil, extra)
			var asked atomic.Bool
			loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				asked.Store(true)
				_, _ = io.WriteString(w, completeAnswer)
			}))
			defer loc.Close()
			var cloudGot string
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				cloudGot = string(b)
				_, _ = io.WriteString(w, "cloud")
			}))
			defer cloud.Close()
			var calls atomic.Int32
			g := NewAnthropic()
			g.Upstream = cloud.URL
			g.Shorten = fakeShortener(&calls)
			l := &Local{BaseURL: loc.URL + "/v1", Model: "qwen", Exclude: tc.install}
			g.Local = func() *Local {
				if tc.none {
					return nil
				}
				return l
			}
			var meta Meta
			g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
			ctx := WithShortenPrompts(WithExclude(sessionCtx(t, "sess"), tc.org), true)
			rec := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(in))
			r.Header.Set("X-Api-Key", "sk-ant-x")
			g.Handler("/v1/messages")(rec, r)
			assert.Equal(t, rec.Body.String(), "cloud")
			assert.Equal(t, meta.Fallthrough, tc.reason)
			assert.False(t, asked.Load(), "no local answer")
			assert.Equal(t, cloudGot, in, "sent as the agent sent it")
			assert.False(t, meta.Shortened)
			assert.Equal(t, calls.Load(), int32(0), "the shortener is never asked")
		})
	}
}

func FuzzUserTexts(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"a\"b\\u00e9"},{"type":"tool_result","content":"x"}]}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","role":"user","content":"x"}]}`))
	f.Add([]byte(` { "messages" : [ { "content" : [ { "text" : "x" , "type" : "text" } ] , "role" : "user" } ] } `))
	f.Fuzz(func(t *testing.T, b []byte) {
		spans, err := userTexts(b)
		if err != nil {
			return
		}
		prev := 0
		for _, sp := range spans {
			if sp.start < prev || sp.end > len(b) || sp.start >= sp.end {
				t.Fatalf("span %d-%d out of order", sp.start, sp.end)
			}
			prev = sp.end
			var s string
			if err := json.Unmarshal(b[sp.start:sp.end], &s); err != nil {
				t.Fatalf("span is not a JSON string: %v", err)
			}
			if s != sp.text {
				t.Fatalf("decoded text differs")
			}
			// Splicing a re-encoded text keeps the body valid JSON with
			// the same value there.
			lit, err := jsonString(sp.text)
			if err != nil {
				t.Fatal(err)
			}
			out := append(append(append([]byte{}, b[:sp.start]...), lit...), b[sp.end:]...)
			if !json.Valid(out) {
				t.Fatalf("splice broke the body")
			}
		}
	})
}
