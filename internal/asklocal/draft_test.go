package asklocal

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestDraftRequestValidate(t *testing.T) {
	files := []File{{Path: "a.go", Start: 1, End: 2, Text: "package a\n\n"}}
	for _, tc := range []struct {
		name string
		req  DraftRequest
		bad  bool
	}{
		{name: "ok", req: DraftRequest{Task: "write Sum", Files: files}},
		{name: "target range", req: DraftRequest{Task: "write Sum", Target: "a.go:1-2", Files: files}},
		{name: "new file", req: DraftRequest{Task: "write Sum", Target: "pkg/new.go", Files: files}},
		{name: "empty task", req: DraftRequest{Task: "  ", Files: files}, bad: true},
		{name: "long task", req: DraftRequest{Task: strings.Repeat("x", MaxTask+1), Files: files}, bad: true},
		{name: "no files", req: DraftRequest{Task: "write Sum"}, bad: true},
		{name: "absolute target", req: DraftRequest{Task: "t", Target: "/etc/passwd", Files: files}, bad: true},
		{name: "escaping target", req: DraftRequest{Task: "t", Target: "../x.go", Files: files}, bad: true},
		{name: "unclean target", req: DraftRequest{Task: "t", Target: "a/./b.go", Files: files}, bad: true},
		{name: "flag target", req: DraftRequest{Task: "t", Target: "-x.go", Files: files}, bad: true},
		{name: "bad range", req: DraftRequest{Task: "t", Target: "a.go:5-2", Files: files}, bad: true},
		{name: "escaping file", req: DraftRequest{Task: "t", Files: []File{{Path: "../a.go", Start: 1, End: 1, Text: "x"}}}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if tc.bad {
				assert.ErrorIs(t, err, ErrInvalid)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseDraft(t *testing.T) {
	for _, tc := range []struct {
		name, answer      string
		lang, code, notes string
		none              bool
	}{
		{
			name:   "block and notes",
			answer: "```go\nfunc Sum(a, b int) int { return a + b }\n```\nNOTES: follows a.go:3.",
			lang:   "go", code: "func Sum(a, b int) int { return a + b }", notes: "follows a.go:3.",
		},
		{
			name:   "numbers copied from the input",
			answer: "```Go\n3| x := 1\n4| y := 2\n```",
			lang:   "go", code: "x := 1\ny := 2",
		},
		{
			name:   "only some lines numbered",
			answer: "```\n3| x := 1\ny := 2\n```",
			code:   "3| x := 1\ny := 2",
		},
		{name: "refusal", answer: "CANNOT DRAFT: the task names no type", notes: "the task names no type", none: true},
		{name: "prose", answer: "I would add a function.", notes: "I would add a function.", none: true},
		{name: "empty block", answer: "```go\n\n```\nNOTES: nothing", lang: "go", notes: "nothing", none: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lang, code, notes, err := ParseDraft(tc.answer)
			if tc.none {
				assert.ErrorIs(t, err, ErrNoDraft)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.lang, lang)
			assert.Equal(t, tc.code, code)
			assert.Equal(t, tc.notes, notes)
		})
	}
}

func TestIsDiffAndIsGo(t *testing.T) {
	assert.True(t, IsDiff("diff", "anything"))
	assert.True(t, IsDiff("", "diff --git a/x b/x\n"))
	assert.True(t, IsDiff("", "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n"))
	assert.False(t, IsDiff("go", "func f() {}\n--- not a diff"))

	assert.True(t, IsGo("go", ""))
	assert.True(t, IsGo("golang", "x.py"))
	assert.True(t, IsGo("", "pkg/a.go:3-9"))
	assert.False(t, IsGo("python", "pkg/a.go"))
	assert.False(t, IsGo("", "a.py"))
}

func TestCheckDiff(t *testing.T) {
	files := []File{{Path: "pkg/a.go", Start: 10, End: 13, Text: "func A() int {\n\tx := 1\n\treturn x\n}\n"}}
	for _, tc := range []struct {
		name, diff, target string
		want               []string // substrings, one per problem
	}{
		{
			name: "applies",
			diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -11,2 +11,2 @@\n-\tx := 1\n+\tx := 2\n \treturn x\n",
		},
		{
			name: "new target file",
			diff: "--- /dev/null\n+++ b/pkg/b.go\n@@ -0,0 +1,2 @@\n+package pkg\n+\n", target: "pkg/b.go",
		},
		{
			name: "context does not match",
			diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -11,2 +11,2 @@\n-\tx := 9\n+\tx := 2\n \treturn x\n",
			want: []string{"pkg/a.go:11 does not match"},
		},
		{
			name: "line not sent",
			diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -40,1 +40,1 @@\n-x\n+y\n",
			want: []string{"pkg/a.go:40 was not among the lines sent"},
		},
		{
			name: "file not sent",
			diff: "--- a/pkg/c.go\n+++ b/pkg/c.go\n@@ -1 +1 @@\n-x\n+y\n",
			want: []string{"pkg/c.go was neither sent nor the target", "pkg/c.go:1 was not among the lines sent"},
		},
		{
			name: "escaping path",
			diff: "--- a/../etc/x\n+++ b/../etc/x\n@@ -1 +1 @@\n-x\n+y\n",
			want: []string{"the diff does not parse"}, // gitx.ParseDiff refuses it before the path check
		},
		{
			name: "short hunk",
			diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -11,3 +11,3 @@\n-\tx := 1\n+\tx := 2\n",
			want: []string{"shorter than its header says"},
		},
		{name: "no file", diff: "just text\n", want: []string{"the diff"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDiff(tc.diff, files, tc.target)
			require.Len(t, got, len(tc.want), "problems: %v", got)
			for i, w := range tc.want {
				assert.Contains(t, got[i], w)
			}
		})
	}
}

func TestCheckGo(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		bad        string
	}{
		{name: "file", code: "package a\n\nfunc A() {}\n"},
		{name: "declarations", code: "func A() int {\n\treturn 1\n}\n\nvar x = 2\n"},
		{name: "statements", code: "x := 1\nif x > 0 {\n\tx++\n}\n"},
		{name: "test table", code: "for _, tc := range []struct{ in, want int }{\n\t{1, 2},\n} {\n\t_ = tc\n}\n"},
		{name: "broken function", code: "func A() int {\n\treturn 1 +\n}\n", bad: "line 3"},
		{name: "broken file", code: "package a\nfunc {\n", bad: "line 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckGo(tc.code)
			if tc.bad == "" {
				assert.Len(t, got, 0, "problems: %v", got)
				return
			}
			require.NotEmpty(t, got)
			assert.Contains(t, got[0], "go syntax: "+tc.bad)
		})
	}
}

func FuzzParseDraft(f *testing.F) {
	f.Add("```go\nfunc A() {}\n```\nNOTES: a.go:1")
	f.Add("CANNOT DRAFT: no")
	f.Add("```\n1| x\n2| y\n```")
	f.Add("``````")
	f.Fuzz(func(t *testing.T, answer string) {
		lang, code, _, err := ParseDraft(answer)
		if err == nil && strings.TrimSpace(code) == "" {
			t.Fatalf("a draft without code: %q", answer)
		}
		if strings.ContainsAny(lang, " \n") {
			t.Fatalf("language %q", lang)
		}
		_ = IsDiff(lang, code)
		_ = CheckGo(code)
	})
}

func FuzzCheckDiff(f *testing.F) {
	f.Add("--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,2 @@\n-x\n+y\n z\n")
	f.Add("--- /dev/null\n+++ b/n.go\n@@ -0,0 +1 @@\n+x\n")
	f.Add("@@ -1 +1 @@\n\\ No newline at end of file\n")
	f.Add("diff --git a/a.go b/a.go\n@@ -99999999999999999999 +1 @@\n")
	files := []File{{Path: "a.go", Start: 1, End: 2, Text: "x\nz\n"}}
	f.Fuzz(func(t *testing.T, diff string) {
		if got := CheckDiff(diff, files, "n.go"); len(got) > maxProblems {
			t.Fatalf("%d problems, more than %d", len(got), maxProblems)
		}
	})
}
