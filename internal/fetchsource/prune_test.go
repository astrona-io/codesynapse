package fetchsource

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const doc = "Client library\n\nIntro text.\n\n" +
	"# Client\n\nOverview.\n\n" +
	"## Install\n\nRun `go get example.com/client`.\n\n" +
	"## Timeouts\n\nRequests wait.\n\n```go\nc := New()\n\n# not a heading\nc.Do()\n```\n\n" +
	"### Retries\n\nThree times.\n\n" +
	"## Transport\n\nSends requests.\n\n```go\nfunc (c *Client) RoundTrip(r *Request) (*Response, error)\n```\n\n" +
	"## Errors\n\nWhat can go wrong, with `ErrClosed`.\n\nMore prose about pools.\n"

func TestPruneMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name, question string
		want, gone     []string
		kept           int
		pruned         bool
	}{
		{
			name: "no question", question: "",
			want: []string{"Intro text.", "## Install", "## Errors"}, kept: 6,
		},
		{
			name: "heading match keeps the subtree, the headings above and declarations", question: "How do timeouts work?",
			want: []string{"Client library", "Intro text.", "# Client", "## Timeouts", "c.Do()", "# not a heading", "### Retries", "Three times.", "## Transport", "func (c *Client) RoundTrip"},
			gone: []string{"Overview.", "## Install", "Sends requests.", "## Errors"},
			kept: 2, pruned: true,
		},
		{
			name: "inline code match", question: "ErrClosed",
			want: []string{"## Errors", "More prose about pools."},
			gone: []string{"## Timeouts", "Requests wait."},
			kept: 1, pruned: true,
		},
		{
			name: "fenced code match never splits the block", question: "New",
			want: []string{"## Timeouts", "```go\nc := New()\n\n# not a heading\nc.Do()\n```"},
			kept: 2, pruned: true,
		},
		{
			name: "prose fallback", question: "pools",
			want: []string{"## Errors", "More prose about pools."},
			gone: []string{"## Install"},
			kept: 1, pruned: true,
		},
		{
			name: "nothing matches: whole page", question: "kubernetes",
			want: []string{"## Install", "Overview."}, kept: 6,
		},
		{
			name: "stop words only: whole page", question: "how do I use the",
			want: []string{"## Install"}, kept: 6,
		},
		{
			name: "short terms end the word", question: "go",
			want: []string{"## Install"}, gone: []string{"## Timeouts"}, kept: 1, pruned: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PruneMarkdown(doc, tc.question)
			assert.Equal(t, p.Sections, 6)
			assert.Equal(t, p.Kept, tc.kept)
			assert.Equal(t, p.Pruned, tc.pruned)
			for _, w := range tc.want {
				assert.Contains(t, p.Text, w)
			}
			for _, g := range tc.gone {
				assert.NotContains(t, p.Text, g)
			}
		})
	}
}

func TestPruneMarkdownWithoutHeadings(t *testing.T) {
	p := PruneMarkdown("just text\nabout timeouts", "timeouts")
	assert.Equal(t, p, Pruned{Text: "just text\nabout timeouts"})
}

func TestPruneMarkdownBounds(t *testing.T) {
	var b strings.Builder
	for range maxSections + 10 {
		b.WriteString("## part\n\nx\n\n")
	}
	b.WriteString("## needle\n\nfound\n")
	p := PruneMarkdown(b.String(), "needle")
	assert.Equal(t, p.Sections, maxSections, "headings beyond the cap stay in the last section")
	assert.Contains(t, p.Text, "found")

	var sigs strings.Builder
	sigs.WriteString("## match\n\n")
	for range maxSignatures + 5 {
		sigs.WriteString("## other\n\n```\nfunc F()\n```\n\n")
	}
	p = PruneMarkdown(sigs.String(), "match")
	assert.Equal(t, strings.Count(p.Text, "func F()"), maxSignatures)
}

func TestTerms(t *testing.T) {
	assert.Equal(t, Terms("How do I set the http.Client Timeout, timeout?"), []string{"set", "http", "client", "timeout"})
	long := strings.Repeat("word ", 100)
	assert.Equal(t, Terms(long), []string{"word"})
	many := ""
	for i := range 40 {
		many += string(rune('a'+i%26)) + string(rune('a'+i/26)) + " "
	}
	assert.Len(t, Terms(many), maxTerms)
}

func TestContainsWord(t *testing.T) {
	for _, tc := range []struct {
		hay, term string
		want      bool
	}{
		{"use go here", "go", true},
		{"good", "go", false},
		{"ago", "go", false},
		{"go", "go", true},
		{"clientconn", "client", true},
		{"newclient", "client", false},
		{"x_client", "client", false},
		{"é go", "go", true},
	} {
		assert.Equal(t, containsWord(tc.hay, tc.term), tc.want, tc.hay+"/"+tc.term)
	}
}

func FuzzPruneMarkdown(f *testing.F) {
	f.Add(doc, "timeouts")
	f.Add("# a\n```\n# b\n", "b")
	f.Add("####### x\n# \n#\n", "x")
	f.Add("- a\n- b\n# h\n- a\n- b\n", "h")
	f.Add("\xff# \xfe\n", "\xff")
	f.Fuzz(func(t *testing.T, text, question string) {
		p := PruneMarkdown(text, question)
		if utf8.ValidString(text) && !utf8.ValidString(p.Text) {
			t.Fatalf("invalid UTF-8 out of %q", text)
		}
		if p.Kept > p.Sections || p.Sections > maxSections {
			t.Fatalf("kept %d of %d sections", p.Kept, p.Sections)
		}
		if !p.Pruned {
			return
		}
		// Only whole lines of the page come out (invalid UTF-8 replaced), never anything new.
		valid := strings.ToValidUTF8(text, "\uFFFD")
		for line := range strings.SplitSeq(p.Text, "\n") {
			require.True(t, strings.Contains(valid, line), "line %q is not in the page", line)
		}
		if len(p.Text) > 3*len(text)+4*(p.Sections+maxSignatures+1) { // a replacement character is 3 bytes
			t.Fatalf("%d bytes out of %d", len(p.Text), len(text))
		}
	})
}
