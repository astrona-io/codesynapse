package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHandlerShortensOnlyWhenTurnedOn(t *testing.T) {
	in := string(body(t, msg{"user", prose}))
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off by default", true: "on"}[on], func(t *testing.T) {
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
			if on {
				ctx = WithShortenPrompts(ctx, true)
			}
			r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(in))
			r.Header.Set("X-Api-Key", "sk-ant-x")
			g.Handler("/v1/messages")(httptest.NewRecorder(), r)
			if !on {
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
