package fetchsource

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

const extractPage = "# Client\n\nUse `NewClient` to connect.\n\n```go\nc, err := NewClient(ctx, addr)\nif err != nil {\n\treturn err\n}\n```\n"

func TestCheckSnippets(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		dropped            int
	}{
		{name: "prose only", answer: "Call it with a context.", want: "Call it with a context."},
		{name: "inline from the page", answer: "Use `NewClient`.", want: "Use `NewClient`."},
		{name: "inline invented", answer: "Use `Dial` or `NewClient`.", want: "Use " + droppedCode + " or `NewClient`.", dropped: 1},
		{name: "block from the page, indentation differs", answer: "Like this:\n```go\nc, err := NewClient(ctx, addr)\n  if err != nil {\nreturn err\n}\n```\nDone.", want: "Like this:\n```go\nc, err := NewClient(ctx, addr)\n  if err != nil {\nreturn err\n}\n```\nDone."},
		{name: "block with an invented line", answer: "```go\nc, err := NewClient(ctx, addr, WithRetry())\n```\nDone.", want: droppedCode + "\nDone.", dropped: 1},
		{name: "unclosed block", answer: "```\nmade up\n", want: droppedCode, dropped: 1},
		{name: "unclosed inline", answer: "a `b", want: "a `b"},
		{name: "empty block", answer: "```\n```", want: "```\n```"},
		{name: "double backticks invented", answer: "call ``rm -rf /`` now", want: "call " + droppedCode + " now", dropped: 1},
		{name: "double backticks from the page", answer: "use ``NewClient`` here", want: "use ``NewClient`` here"},
		{name: "span holding a single backtick", answer: "see ``a ` b``", want: "see " + droppedCode, dropped: 1},
		{name: "unmatched run is literal", answer: "a ``` b `NewClient` c", want: "a ``` b `NewClient` c"},
		{name: "unmatched run then invented span", answer: "a `` b `Dial` c", want: "a `` b " + droppedCode + " c", dropped: 1},
		{name: "span across lines", answer: "use `NewClient\nDial` now", want: "use " + droppedCode + " now", dropped: 1},
		{name: "longer closing fence", answer: "```\nreturn err\n`````\nok", want: "```\nreturn err\n`````\nok"},
		{name: "info string does not close", answer: "```\nmade up\n```go\n", want: droppedCode, dropped: 1},
		{name: "tilde fence invented", answer: "~~~~\nrm -rf /\n~~~~\nok", want: droppedCode + "\nok", dropped: 1},
		{name: "indented block from the page", answer: "Like this:\n\n    c, err := NewClient(ctx, addr)\n\n    return err\n\nDone.", want: "Like this:\n\n    c, err := NewClient(ctx, addr)\n\n    return err\n\nDone."},
		{name: "indented block invented", answer: "Like this:\n\n    rm -rf /\n\nDone.", want: "Like this:\n\n" + droppedCode + "\n\nDone.", dropped: 1},
		{name: "tab indented block invented", answer: "\trm -rf /", want: droppedCode, dropped: 1},
		{name: "indented line continuing a paragraph", answer: "Some text\n    more text", want: "Some text\n    more text"},
		{name: "list continuation is not code", answer: "- item\n\n  more about it", want: "- item\n\n  more about it"},
		{name: "indented code inside a list", answer: "- item\n\n      rm -rf /", want: "- item\n\n" + droppedCode, dropped: 1},
		{name: "html code tags stripped", answer: "run <code>rm -rf /</code> or <PRE class=x>evil</pre>", want: "run  rm -rf /  or  evil "},
		{name: "html tags in a span kept", answer: "the `<pre>` tag", want: "the " + droppedCode + " tag", dropped: 1},
		{name: "other tags kept", answer: "a <b>bold</b> <pretty>", want: "a <b>bold</b> <pretty>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := CheckSnippets(tc.answer, extractPage)
			assert.Equal(t, got, tc.want)
			assert.Equal(t, n, tc.dropped)
		})
	}
}

func TestCheckSnippetsBounded(t *testing.T) {
	answer := strings.Repeat("`x` ", 2*maxSnippets)
	got, n := CheckSnippets(answer, "")
	assert.Equal(t, n, maxSnippets+1, "the rest of the answer is cut")
	assert.Equal(t, strings.Count(got, droppedCode), n)
	assert.NotContains(t, got, "`x`")
}

func TestExtractRequestValidate(t *testing.T) {
	ok := ExtractRequest{Question: "how to connect", Text: extractPage}
	assert.NoError(t, ok.Validate())
	for name, r := range map[string]ExtractRequest{
		"no question":   {Text: extractPage},
		"long question": {Question: strings.Repeat("a", MaxQuestion+1), Text: extractPage},
		"no text":       {Question: "q", Text: "  "},
		"long text":     {Question: "q", Text: strings.Repeat("a", MaxExtractInput+1)},
		"bad utf-8":     {Question: "q", Text: "a\xffb"},
	} {
		assert.Error(t, r.Validate(), name)
	}
}

func FuzzCheckSnippets(f *testing.F) {
	f.Add("Use `NewClient`.\n```go\nNewClient(ctx)\n```", extractPage)
	f.Add("```\n`a`\n~~~\n```", "a")
	f.Add("call ``rm -rf /`` now ``` `x", "x")
	f.Add("- item\n\n      code\n\n\tmore\n<pre>p</pre>", "code")
	f.Add("````\n```\n````", "")
	f.Fuzz(func(t *testing.T, answer, sent string) {
		if len(answer) > 1<<16 || len(sent) > 1<<16 {
			return
		}
		got, n := CheckSnippets(answer, sent)
		if n < 0 || n > maxSnippets+1 {
			t.Fatalf("dropped %d", n)
		}
		if utf8.ValidString(answer) && !utf8.ValidString(got) {
			t.Fatal("valid UTF-8 in, invalid out")
		}
		// Without anything Markdown or HTML could show as code, nothing changes.
		if !strings.ContainsAny(answer, "`~<\t") && !strings.Contains(answer, "    ") && got != strings.TrimRight(answer, "\n") {
			t.Fatalf("an answer without code changed: %q -> %q", answer, got)
		}
		// Every code span of the result is in sent: an invented span never
		// survives, whatever the backtick runs around it.
		if !strings.ContainsAny(answer, "~<\t") && !strings.Contains(answer, "    ") && !strings.Contains(answer, "\n") {
			for _, code := range spans(got) {
				if c := strings.TrimSpace(code); c != "" && !strings.Contains(sent, c) {
					t.Fatalf("span %q not in the page survived: %q -> %q", code, answer, got)
				}
			}
		}
	})
}

// spans lists the code spans of one line by the backtick-run rule.
func spans(s string) []string {
	var out []string
	for p := 0; p < len(s); {
		if s[p] != '`' {
			p++
			continue
		}
		q := p
		for q < len(s) && s[q] == '`' {
			q++
		}
		end := closingRun(s, q, q-p)
		if end < 0 {
			p = q
			continue
		}
		out = append(out, s[q:end])
		p = end + q - p
	}
	return out
}
