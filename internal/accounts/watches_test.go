package accounts

import (
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestWatchExcludes(t *testing.T) {
	tooMany := make([]string, MaxWatchExcludes+1)
	for i := range tooMany {
		tooMany[i] = "x" + strings.Repeat("y", i)
	}
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		bad  bool
	}{
		{name: "none", in: nil, want: []string{}},
		{name: "lowercased, sorted, once", in: []string{" Test-* ", "*-archive", "test-*"}, want: []string{"*-archive", "test-*"}},
		{name: "glob in the middle", in: []string{"*legacy*", "a*b"}, want: []string{"*legacy*", "a*b"}},
		{name: "at the limit", in: []string{strings.Repeat("a", MaxWatchPattern)}, want: []string{strings.Repeat("a", MaxWatchPattern)}},
		{name: "too many", in: tooMany, bad: true},
		{name: "too long", in: []string{strings.Repeat("a", MaxWatchPattern+1)}, bad: true},
		{name: "empty", in: []string{" "}, bad: true},
		{name: "only stars", in: []string{"**"}, bad: true},
		{name: "slash", in: []string{"acme/api"}, bad: true},
		{name: "question mark", in: []string{"api?"}, bad: true},
		{name: "class", in: []string{"[a-z]*"}, bad: true},
		{name: "space inside", in: []string{"my repo"}, bad: true},
		{name: "not ascii", in: []string{"tëst-*"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WatchExcludes(tc.in)
			if tc.bad {
				require.True(t, errors.Is(err, ErrInvalidInput), "want ErrInvalidInput")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, got, tc.want)
		})
	}
}

func TestWatchExcluded(t *testing.T) {
	patterns := []string{"*-archive", "test-*", "*legacy*", "exact"}
	for name, want := range map[string]bool{
		"test-api":      true,
		"TEST-API":      true,
		"docs-archive":  true,
		"old-legacy-ui": true,
		"legacy":        true,
		"exact":         true,
		"Exact":         true,
		"exactly":       false,
		"api":           false,
		"atest-api":     false,
		"archive":       false,
	} {
		assert.Equal(t, WatchExcluded(patterns, name), want, name)
	}
	assert.False(t, WatchExcluded(nil, "api"), "no patterns exclude nothing")
}

// FuzzWatchExcluded checks the matcher against path.Match, which agrees
// with it for the characters a pattern and a repository name may hold.
func FuzzWatchExcluded(f *testing.F) {
	for _, seed := range [][2]string{{"test-*", "test-api"}, {"*-archive", "x-archive"}, {"a*b*c", "aXbYc"}, {"*a*", "bbb"}, {"**x", "x"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pattern, name string) {
		ps, err := WatchExcludes([]string{pattern})
		if err != nil || len(name) > 100 || strings.ContainsAny(name, `/?[\*`) {
			return
		}
		want, err := path.Match(ps[0], strings.ToLower(name))
		if err != nil {
			return
		}
		if got := WatchExcluded(ps, name); got != want {
			t.Fatalf("WatchExcluded(%q, %q) = %v, path.Match says %v", ps[0], name, got, want)
		}
	})
}
