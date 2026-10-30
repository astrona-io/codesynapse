package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEstimateTokens(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 3: 1, 4: 2, 7: 2, 8: 3, 3500: 1000, 3501: 1001}
	for size, want := range cases {
		assert.Equal(t, want, estimateTokens(size))
	}
}

func TestFit(t *testing.T) {
	const window = 100_000
	// size is chosen so the estimate is exactly 7000 tokens: room is
	// window - 7000 - localMargin.
	const size = 24_500
	room := window - estimateTokens(size) - localMargin
	cases := map[string]struct {
		window, maxOutput, size, maxTokens int
		held                               bool
		want                               int
		fits                               bool
	}{
		"no window, at the cap":          {0, 0, unknownWindowBody, 100, false, 100, true},
		"no window, over the cap":        {0, 0, unknownWindowBody + 1, 100, false, 0, false},
		"over the hard body bound":       {1 << 30, 0, maxLocalBody + 1, 100, true, 0, false},
		"at the hard body bound":         {1 << 30, 0, maxLocalBody, 100, true, 100, true},
		"answer fills the room exactly":  {window, 0, size, room, false, room, true},
		"answer one over, streamed":      {window, 0, size, room + 1, false, 0, false},
		"answer one over, no max output": {window, 0, size, room + 1, true, 0, false},
		"held, lowered to the room":      {window, room, size, 128_000, true, room, true},
		"held, max output one over room": {window, room + 1, size, 128_000, true, 0, false},
		"held, own max_tokens is less":   {window, 1 << 20, size, room + 1, true, 0, false},
		"no max_tokens":                  {window, 0, size, 0, false, 0, true},
		"negative max_tokens":            {window, 0, size, -5, false, 0, true},
		"prompt fills the window":        {estimateTokens(size) + localMargin, 0, size, 0, false, 0, false},
		"prompt leaves one token":        {estimateTokens(size) + localMargin + 1, 0, size, 1, false, 1, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			l := &Local{ContextWindow: tc.window, MaxOutput: tc.maxOutput}
			got, fits := l.fit(tc.size, tc.maxTokens, tc.held)
			assert.Equal(t, tc.fits, fits)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestTranslateFitsWhatIsSent checks the fit on the translated request: the
// max_tokens sent is lowered for a held answer, a streamed one is refused,
// and thinking blocks the local model never sees do not count.
func TestTranslateFitsWhatIsSent(t *testing.T) {
	const opus, haiku = "claude-opus-5-5", "claude-haiku-5-5"
	text := strings.Repeat("func f() {}\n", 3000) // 36 KB, about 10 300 tokens
	body := func(model string, maxTokens int, extra ...any) []byte {
		msgs := []any{map[string]any{"role": "user", "content": text}}
		msgs = append(msgs, extra...)
		b, err := json.Marshal(map[string]any{"model": model, "max_tokens": maxTokens, "messages": msgs})
		require.NoError(t, err)
		return b
	}
	l := &Local{Model: "qwen", ContextWindow: 20_000, MaxOutput: 4096}

	t.Run("held answer is lowered to the room", func(t *testing.T) {
		_, out, reason := l.translate(t.Context(), body(opus, 64_000))
		require.Equal(t, "", reason)
		var sent struct {
			MaxTokens int `json:"max_tokens"`
		}
		require.NoError(t, json.Unmarshal(out, &sent))
		assert.True(t, sent.MaxTokens >= l.MaxOutput && sent.MaxTokens < 64_000)
		assert.True(t, estimateTokens(len(out))+sent.MaxTokens+localMargin <= l.ContextWindow+8)
	})
	t.Run("a max_tokens that fits is sent unchanged", func(t *testing.T) {
		_, out, reason := l.translate(t.Context(), body(opus, 1000))
		require.Equal(t, "", reason)
		assert.Contains(t, string(out), `"max_tokens":1000}`)
	})
	t.Run("streamed answer is not lowered", func(t *testing.T) {
		_, out, reason := l.translate(t.Context(), body(haiku, 64_000))
		assert.Equal(t, FallTooLarge, reason)
		assert.True(t, out == nil)
	})
	t.Run("max output does not fit", func(t *testing.T) {
		small := &Local{Model: "qwen", ContextWindow: 14_000, MaxOutput: 4096}
		_, _, reason := small.translate(t.Context(), body(opus, 64_000))
		assert.Equal(t, FallTooLarge, reason)
	})
	t.Run("thinking blocks are not counted", func(t *testing.T) {
		sig := strings.Repeat("A", 200_000) // far past the window on its own
		b := body(opus, 1000,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "hm", "signature": sig},
				map[string]any{"type": "text", "text": "ok"},
			}},
			map[string]any{"role": "user", "content": "go on"})
		_, out, reason := l.translate(t.Context(), b)
		require.Equal(t, "", reason)
		assert.True(t, len(out) < len(b)/4)
	})
}
