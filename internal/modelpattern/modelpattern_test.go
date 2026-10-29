package modelpattern

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestNormalize(t *testing.T) {
	many := make([]string, MaxPatterns+1)
	for i := range many {
		many[i] = "m" + strings.Repeat("x", i+1)
	}
	cases := map[string]struct {
		in   []string
		want []string
		bad  bool
	}{
		"nil":               {nil, []string{}, false},
		"exact and prefix":  {[]string{"claude-opus-*", "gpt-5"}, []string{"claude-opus-*", "gpt-5"}, false},
		"lower, trim, sort": {[]string{"  Claude-Sonnet-5 ", "CLAUDE-OPUS-*"}, []string{"claude-opus-*", "claude-sonnet-5"}, false},
		"deduplicated":      {[]string{"a", "A", "a "}, []string{"a"}, false},
		"empty lines":       {[]string{"", "  ", "qwen3:8b"}, []string{"qwen3:8b"}, false},
		"all punctuation":   {[]string{"a.b:c_d-e*"}, []string{"a.b:c_d-e*"}, false},
		"lone star":         {[]string{"*"}, nil, true},
		"star inside":       {[]string{"claude-*-5"}, nil, true},
		"two stars":         {[]string{"claude**"}, nil, true},
		"slash":             {[]string{"anthropic/claude"}, nil, true},
		"space inside":      {[]string{"claude opus"}, nil, true},
		"non-ascii":         {[]string{"clаude"}, nil, true},
		"too long":          {[]string{strings.Repeat("a", MaxLen+1)}, nil, true},
		"longest":           {[]string{strings.Repeat("a", MaxLen-1) + "*"}, []string{strings.Repeat("a", MaxLen-1) + "*"}, false},
		"too many":          {many, nil, true},
		"many duplicates":   {append(slicesRepeat("same", MaxPatterns+5), "other"), []string{"other", "same"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if tc.bad {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, got, tc.want)
		})
	}
}

func slicesRepeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestMatch(t *testing.T) {
	list := []string{"claude-opus-*", "gpt-5"}
	cases := map[string]bool{
		"claude-opus-5-5":   true,
		"Claude-Opus-4-1":   true,
		"gpt-5":             true,
		"gpt-5-mini":        false,
		"claude-sonnet-5-5": false,
		"":                  false,
	}
	for model, want := range cases {
		assert.Equal(t, Match(list, model), want, model)
	}
	assert.False(t, Match(nil, "claude-opus-5-5"), "an empty list excludes nothing")
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"claude-opus-*", "gpt-5", "*", "a\nb", "ÄÖ", strings.Repeat("z", 70)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := Normalize(strings.Split(s, "\n"))
		if err != nil {
			return
		}
		if len(out) > MaxPatterns {
			t.Fatalf("%d patterns kept", len(out))
		}
		for i, p := range out {
			if !valid(p) || p != strings.ToLower(p) {
				t.Fatalf("kept invalid pattern %q", p)
			}
			if i > 0 && out[i-1] >= p {
				t.Fatalf("not sorted and unique: %q", out)
			}
		}
		again, err := Normalize(out)
		if err != nil || strings.Join(again, "\n") != strings.Join(out, "\n") {
			t.Fatalf("not idempotent: %q -> %q (%v)", out, again, err)
		}
	})
}
