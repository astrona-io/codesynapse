package fixes

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestValidateRewrite(t *testing.T) {
	const src = "// Copyright Example\npackage app\n\nimport \"fmt\"\n\n// Run prints a command.\nfunc Run(cmd string) {\n\tfmt.Print(cmd)\n}\n\nfunc Keep() { fmt.Print(\"keep\") }\n"
	fixed := strings.Replace(src, "fmt.Print(cmd)", "fmt.Println(cmd)", 1)
	comment := func(line int) reviews.CommentItem {
		return reviews.CommentItem{File: "a.go", Line: line, Status: reviews.Accepted}
	}
	for _, tt := range []struct {
		name     string
		file     string
		orig     string
		out      string
		comments []reviews.CommentItem
		wantOK   bool
	}{
		{name: "function body", out: fixed, wantOK: true},
		{name: "additional body lines", out: strings.Replace(src, "fmt.Print(cmd)", "if cmd != \"\" {\n\t\tfmt.Print(cmd)\n\t}", 1), wantOK: true},
		{name: "signature comment", out: fixed, comments: []reviews.CommentItem{comment(7)}, wantOK: true},
		{name: "line range", out: fixed, comments: []reviews.CommentItem{{File: "a.go", Line: 7, EndLine: 9, Status: reviews.Accepted}}, wantOK: true},
		{name: "repeated text does not broaden scope", out: strings.ReplaceAll(src, "fmt.Print", "fmt.Println")},
		{name: "both accepted functions", out: strings.ReplaceAll(src, "fmt.Print", "fmt.Println"), comments: []reviews.CommentItem{comment(8), comment(11)}, wantOK: true},
		{name: "syntax error", out: strings.Replace(fixed, "fmt.Println(cmd)", "fmt.Println(", 1)},
		{name: "malformed original", orig: "package app\nfunc Run( {", out: fixed},
		{name: "license removal", out: strings.TrimPrefix(fixed, "// Copyright Example\n")},
		{name: "doc comment removal", out: strings.Replace(fixed, "// Run prints a command.\n", "", 1)},
		{name: "package change", out: strings.Replace(fixed, "package app", "package other", 1)},
		{name: "import addition", out: strings.Replace(fixed, "import \"fmt\"", "import \"fmt\"\nimport \"os\"", 1)},
		{name: "signature change", out: strings.Replace(fixed, "cmd string", "cmd any", 1)},
		{name: "renamed target", out: strings.Replace(fixed, "func Run", "func Changed", 1)},
		{name: "unrelated declaration", out: strings.Replace(fixed, "\"keep\"", "\"changed\"", 1)},
		{name: "new declaration", out: fixed + "func Added() {}\n"},
		{name: "removed declaration", out: strings.Replace(fixed, "func Keep() { fmt.Print(\"keep\") }\n", "", 1)},
		{name: "trailing comment", out: fixed + "// changed\n"},
		{name: "newline removal", out: strings.TrimSuffix(fixed, "\n")},
		{name: "unsupported Python", file: "a.py", orig: "def run():\n    pass\n", out: "def run():\n    return\n"},
		{name: "unsupported YAML", file: "a.yaml", orig: "a: 1\n", out: "a: 2\n"},
		{name: "unsupported JSON", file: "a.json", orig: "{\"a\":1}", out: "{\"a\":2}"},
		{name: "missing comments", out: fixed, comments: []reviews.CommentItem{}},
		{name: "missing line", out: fixed, comments: []reviews.CommentItem{comment(0)}},
		{name: "foreign file", out: fixed, comments: []reviews.CommentItem{{File: "other.go", Line: 8, Status: reviews.Accepted}}},
		{name: "unaccepted comment", out: fixed, comments: []reviews.CommentItem{{File: "a.go", Line: 8, Status: reviews.Open}}},
		{name: "reversed range", out: fixed, comments: []reviews.CommentItem{{File: "a.go", Line: 8, EndLine: 7, Status: reviews.Accepted}}},
		{name: "cross declaration range", out: fixed, comments: []reviews.CommentItem{{File: "a.go", Line: 8, EndLine: 11, Status: reviews.Accepted}}},
		{name: "outside file", out: fixed, comments: []reviews.CommentItem{comment(1000)}},
		{name: "header target", out: fixed, comments: []reviews.CommentItem{comment(1)}},
		{name: "doc comment target", out: fixed, comments: []reviews.CommentItem{comment(6)}},
		{name: "import target", out: fixed, comments: []reviews.CommentItem{comment(4)}},
		{name: "CRLF unchanged", orig: strings.ReplaceAll(src, "\n", "\r\n"), out: strings.ReplaceAll(fixed, "\n", "\r\n"), wantOK: true},
		{name: "CRLF normalization rejected", orig: strings.ReplaceAll(src, "\n", "\r\n"), out: fixed},
		{name: "unicode body and prefix", orig: strings.Replace(src, "Example", "Eksempel 🦊", 1), out: strings.Replace(strings.Replace(fixed, "Example", "Eksempel 🦊", 1), "fmt.Println(cmd)", "fmt.Println(\"héj\", cmd)", 1), wantOK: true},
		{name: "method body", orig: "package app\ntype A struct{}\nfunc (a A) Run() { println(1) }\n", out: "package app\ntype A struct{}\nfunc (a A) Run() { println(2) }\n", comments: []reviews.CommentItem{comment(3)}, wantOK: true},
		{name: "method receiver changed", orig: "package app\ntype A struct{}\nfunc (a A) Run() { println(1) }\n", out: "package app\ntype A struct{}\nfunc (a *A) Run() { println(2) }\n", comments: []reviews.CommentItem{comment(3)}},
		{name: "same line ambiguity", orig: "package app\nfunc A() {}; func B() {}\n", out: "package app\nfunc A() { println(1) }; func B() {}\n", comments: []reviews.CommentItem{comment(2)}},
		{name: "physical line directive", orig: "package app\n//line foreign.go:100\nfunc A() { println(1) }\n", out: "package app\n//line foreign.go:100\nfunc A() { println(2) }\n", comments: []reviews.CommentItem{comment(3)}, wantOK: true},
		{name: "forged directive line", orig: "package app\n//line foreign.go:100\nfunc A() { println(1) }\n", out: "package app\n//line foreign.go:100\nfunc A() { println(2) }\n", comments: []reviews.CommentItem{comment(100)}},
		{name: "oversized response", out: "package app\n/*" + strings.Repeat("x", maxFileBytes) + "*/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.file == "" {
				tt.file = "a.go"
			}
			if tt.orig == "" {
				tt.orig = src
			}
			if tt.comments == nil {
				tt.comments = []reviews.CommentItem{comment(8)}
			}
			err := validateRewrite(tt.file, []byte(tt.orig), []byte(tt.out), tt.comments)
			if tt.wantOK {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestValidateRewriteDoesNotExposeParserInput(t *testing.T) {
	const marker = "private_source_marker"
	err := validateRewrite("a.go", []byte(original), []byte("package app\nfunc "+marker+"("), []reviews.CommentItem{{File: "a.go", Line: 4, Status: reviews.Accepted}})
	if assert.Error(t, err) {
		assert.NotContains(t, err.Error(), marker)
	}
}

func FuzzValidateRewrite(f *testing.F) {
	f.Add(original, strings.Replace(original, `exec("sh", "-c", cmd)`, `exec(cmd)`, 1), 4, 0)
	f.Add("package a\nfunc A() {}\n", "package a\nfunc A() {\nprintln(1)\n}\n", 2, 2)
	f.Add("package a\n//line a.go:100\nfunc A() {}\n", "package a\n", 100, 0)
	f.Fuzz(func(_ *testing.T, orig, out string, line, endLine int) {
		// Malformed input and hostile ranges must never turn AST offsets into
		// an out-of-bounds slice when comparing protected source bytes.
		_ = validateRewrite("a.go", []byte(orig), []byte(out), []reviews.CommentItem{{
			File: "a.go", Line: line, EndLine: endLine, Status: reviews.Accepted,
		}})
	})
}
