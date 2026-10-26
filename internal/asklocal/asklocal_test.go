package asklocal

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct {
		spec       string
		path       string
		start, end int
		bad        bool
	}{
		{spec: "internal/api/api.go", path: "internal/api/api.go"},
		{spec: " a.go:10-80 ", path: "a.go", start: 10, end: 80},
		{spec: "a.go:5-5", path: "a.go", start: 5, end: 5},
		{spec: "a.go:9-3", bad: true},
		{spec: "a.go:0-3", bad: true},
		{spec: "dir/", path: "dir/"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			p, s, e, err := ParseSpec(tc.spec)
			if tc.bad {
				assert.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []any{p, s, e}, []any{tc.path, tc.start, tc.end})
		})
	}
}

func TestValidate(t *testing.T) {
	ok := File{Path: "a.go", Start: 1, End: 2, Text: "a\nb\n"}
	for _, tc := range []struct {
		name string
		req  Request
		good bool
	}{
		{"ok", Request{Question: "what?", Files: []File{ok}}, true},
		{"no question", Request{Question: " ", Files: []File{ok}}, false},
		{"long question", Request{Question: strings.Repeat("q", MaxQuestion+1), Files: []File{ok}}, false},
		{"no files", Request{Question: "q"}, false},
		{"escape", Request{Question: "q", Files: []File{{Path: "../x", Start: 1, End: 1}}}, false},
		{"absolute", Request{Question: "q", Files: []File{{Path: "/etc/passwd", Start: 1, End: 1}}}, false},
		{"bad range", Request{Question: "q", Files: []File{{Path: "a.go", Start: 3, End: 2}}}, false},
		{"too large", Request{Question: "q", Files: []File{{Path: "a.go", Start: 1, End: 1, Text: strings.Repeat("x", MaxTotalBytes+1)}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.good {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrInvalid)
			}
		})
	}
}

func TestNumbered(t *testing.T) {
	got := Numbered(File{Path: "a.go", Start: 7, End: 8, Text: "x := 1\ny := 2"})
	assert.Equal(t, got, "=== a.go (lines 7-8) ===\n7| x := 1\n8| y := 2\n")
}

func TestCite(t *testing.T) {
	files := []File{{Path: "internal/api/api.go", Start: 10, End: 40}, {Path: "README.md", Start: 1, End: 5}}
	answer := "Routes are added in internal/api/api.go:12-20 and checked at internal/api/api.go:12-20 again (internal/api/api.go:39). " +
		"See README.md:3. Not sent: internal/api/api.go:41, other.go:1. A time 10:30 and a version 1.2 are not citations."
	got, bad := Cite(answer, files)
	assert.Equal(t, got, []Citation{{"internal/api/api.go", 12, 20}, {"internal/api/api.go", 39, 39}, {"README.md", 3, 3}})
	assert.Equal(t, bad, 2)
	short, bad := Cite("See api.go:15 and api/api.go:16-18.", files)
	assert.Equal(t, short, []Citation{{"internal/api/api.go", 15, 15}, {"internal/api/api.go", 16, 18}}, "a shortened path names the file it ends")
	assert.Equal(t, bad, 0)
	two := []File{{Path: "a/x.go", Start: 1, End: 9}, {Path: "b/x.go", Start: 1, End: 9}}
	_, bad = Cite("x.go:3", two)
	assert.Equal(t, bad, 1, "a name two files end with is ambiguous")
	none, bad := Cite("no citations here", files)
	assert.Len(t, none, 0)
	assert.Equal(t, bad, 0)
}
