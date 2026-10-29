package fetchsource

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestHTMLText(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"paragraphs", "<p>one</p><p>two\n   three</p>", "one\ntwo three"},
		{"scripts and styles", "<p>a<script>var x = '<p>no</p>'</script>b</p><style>p{}</style><noscript>enable js</noscript>", "ab"},
		{"svg", "<p>x<svg><text>chart</text></svg>y</p>", "xy"},
		{"headings", "<h2>Install</h2><p>go get</p>", "## Install\n\ngo get"},
		{"lists", "<ul><li>a</li><li>b <b>c</b></li></ul>", "- a\n- b c"},
		{"pre keeps layout", "<pre>if x {\n\treturn\n}</pre>", "```\nif x {\n\treturn\n}\n```"},
		{"entities", "<p>a &lt; b &amp;&amp; c</p>", "a < b && c"},
		{"table", "<table><tr><th>k</th><th>v</th></tr><tr><td>a</td><td>1</td></tr></table>", "| k | v\n| a | 1"},
		{"unclosed skip", "<p>a</p><script>never closed", "a"},
		{"stray end tags", "</script></pre><p>a</p>", "a"},
		{"nested blocks", "<div><div><div><p>a</p></div></div></div><br><br><br><p>b</p>", "a\nb"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, HTMLText([]byte(tc.in)), tc.want)
		})
	}
}

func TestPlainText(t *testing.T) {
	assert.Equal(t, PlainText([]byte("a  \r\nb\n\n\n\nc\xff")), "a\nb\n\nc�")
}

func FuzzHTMLText(f *testing.F) {
	for _, s := range []string{page, "<pre>a</pre>", "<script>", "<svg/><p>x", "&#0;&#xD800;", "<h1><li><td>", "\xff<p>\xfe"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out := HTMLText(in)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out of %q", in)
		}
		// Markers and entity expansion stay within a small factor.
		if len(out) > 4*len(in)+16 {
			t.Fatalf("%d bytes out of %d", len(out), len(in))
		}
		if strings.TrimSpace(out) != out {
			t.Fatalf("untrimmed output %q", out)
		}
	})
}
