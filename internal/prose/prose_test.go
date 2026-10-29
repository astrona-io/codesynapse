package prose

import (
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLint(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		filler   int
		greeting int
	}{
		{"clean", "Returns the user. Call it after login.", 0, 0},
		{"filler", "Note that the cache is shared. In order to reset it, restart.", 2, 0},
		{"case", "AS YOU CAN SEE, it works.", 1, 0},
		{"curly apostrophe", "Let’s look at the store.", 1, 0},
		{"inside a word", "Denote that value; the outlet's plug; deliveries.", 0, 0},
		{"underscore is a word character", "note_that_flag", 0, 0},
		{"greeting and sign-off", "Sure! Here it is.\n\nHope this helps. Happy coding!", 0, 3},
		{"repeated", "note that a; note that b; NOTE THAT c", 3, 0},
		{"punctuated greeting needs its mark", "Hello world example", 0, 0},
		{"headings are not greetings", "## What changed\nThanks are due to nobody.", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, g := Counts(c.text)
			assert.Equal(t, f, c.filler)
			assert.Equal(t, g, c.greeting)
			if c.filler+c.greeting == 0 {
				assert.NoError(t, Check(c.text))
			} else {
				assert.Error(t, Check(c.text))
			}
		})
	}
}

func TestStyleBlocks(t *testing.T) {
	assert.True(t, strings.HasPrefix(Style, NoFiller), "Style includes the no-filler part")
	assert.Equal(t, strings.Count(Style, NoFiller), 1)
	for _, p := range phrases {
		assert.Equal(t, p.text, strings.ToLower(p.text), "phrases are lower-case")
		assert.False(t, strings.Contains(p.text, "’"), "phrases use a plain apostrophe")
	}
}

func TestLintIsBounded(t *testing.T) {
	long := strings.Repeat("x", MaxLintBytes) + " note that"
	assert.Len(t, Lint(long), 0)
}

func FuzzLint(f *testing.F) {
	for _, s := range []string{"", "Note that x", "let’s", "Sure! Hope this helps.", "denote that", "\xff note that \xfe"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hits := Lint(s)
		total := 0
		for _, h := range hits {
			require.True(t, h.Count > 0, "only phrases that occur")
			require.True(t, slices.ContainsFunc(phrases, func(p phrase) bool { return p.text == h.Phrase && p.kind == h.Kind }), "only listed phrases")
			total += h.Count
		}
		require.True(t, total <= len(s), "at most one hit per byte")
		if isASCII(s) {
			require.Equal(t, len(Lint(strings.ToUpper(s))), len(hits), "case does not matter")
		}
	})
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
