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
		if n == 0 && !strings.Contains(answer, "`") && !strings.Contains(answer, "~~~") && got != strings.TrimRight(answer, "\n") {
			t.Fatalf("an answer without code changed: %q -> %q", answer, got)
		}
	})
}
