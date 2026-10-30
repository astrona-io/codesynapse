package fetchsource

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestCleanHTML(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"paragraphs", "<p>one</p><p>two\n   three</p>", "one\ntwo three"},
		{"scripts and styles", "<p>a<script>var x = '<p>no</p>'</script>b</p><style>p{}</style><noscript>enable js</noscript>", "ab"},
		{"svg", "<p>x<svg><text>chart</text></svg>y</p>", "xy"},
		{"headings", "<h2>Install</h2><p>go get</p>", "## Install\n\ngo get"},
		{"lists", "<ul><li>a</li><li>b <b>c</b></li></ul>", "- a\n- b c"},
		{"nested lists", "<ul><li>a<ul><li>b</li></ul></li></ul>", "- a\n  - b"},
		{"pre keeps layout", "<pre>if x {\n\treturn\n}</pre>", "```\nif x {\n\treturn\n}\n```"},
		{"pre language", `<pre><code class="hl language-go">x := 1</code></pre>`, "```go\nx := 1\n```"},
		{"unsafe language ignored", `<pre class="language-a&quot;b">x</pre>`, "```\nx\n```"},
		{"inline code", "<p>call <code>New( )</code> first</p>", "call `New( )` first"},
		{"inline code with a backtick", "<p><code>a`b</code></p>", "a`b"},
		{"entities", "<p>a &lt; b &amp;&amp; c</p>", "a < b && c"},
		{"table", "<table><tr><th>k</th><th>v</th></tr><tr><td>a</td><td>1</td></tr></table>", "| k | v |\n| --- | --- |\n| a | 1 |"},
		{"blocks in cells stay on the row", "<table><tr><td><p>a</p><p>b</p></td><td><ul><li>c</li></ul></td></tr></table>", "| a b | c |"},
		{"chrome", "<nav>menu</nav><aside>ad</aside><footer>legal</footer><form><label>q</label><input></form><p>kept</p>", "kept"},
		{"site header", "<header>Site</header><main><header><h1>Page</h1></header><p>x</p></main>", "# Page\n\nx"},
		{"banner role", `<div role="banner">Site</div><div role="navigation">menu</div><p>x</p>`, "x"},
		{"hidden", `<p hidden>a</p><p aria-hidden="true">b</p><p aria-hidden="false">c</p><div style="color: red; display : none">d</div><span style="visibility:hidden">e</span><p>f</p>`, "c\nf"},
		{"tracking image", `<p>a<img src="https://t.example/p.gif" width="1" height="1" alt="pixel">b</p>`, "ab"},
		{"repeated menu", "<ul><li>Home</li><li>Docs</li></ul><p>x</p><ul><li>Home</li><li>Docs</li></ul>", "- Home\n- Docs\nx"},
		{"unclosed skip", "<p>a</p><script>never closed", "a"},
		{"stray end tags", "</script></pre><p>a</p>", "a"},
		{"nested blocks", "<div><div><div><p>a</p></div></div></div><br><br><br><p>b</p>", "a\nb"},
		{"title", "<title>T</title><p>x</p>", "T\nx"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, CleanHTML([]byte(tc.in)), tc.want)
		})
	}
}

func TestCleanHTMLBounds(t *testing.T) {
	deep := strings.Repeat("<div>", maxDepth+100) + "deep" + strings.Repeat("</div>", maxDepth+100)
	assert.Equal(t, CleanHTML([]byte("<p>top</p>"+deep)), "top", "content below the depth cap is dropped")
	// Deeper than the parser takes: the text survives without structure.
	deeper := "<script>x()</script><img src=x><p>top</p>" + strings.Repeat("<div>", 1000) + "deep" + strings.Repeat("</div>", 1000)
	assert.Equal(t, CleanHTML([]byte(deeper)), "top deep")
	lists := strings.Repeat("<ul><li>", 50) + "x"
	out := CleanHTML([]byte(lists))
	assert.True(t, strings.Contains(out, strings.Repeat("  ", maxIndent)+"- x"))
	assert.False(t, strings.Contains(out, strings.Repeat("  ", maxIndent+1)+"- x"), "indentation is capped")
	big := []byte("<p>" + strings.Repeat("a", maxCleanInput) + "</p><p>after</p>")
	assert.NotContains(t, CleanHTML(big), "after", "input beyond the cap is not read")
}

func TestClean(t *testing.T) {
	assert.Equal(t, Clean("text/markdown", []byte("- a\n- b\n\nx\n\n- a\n- b")), "- a\n- b\n\nx")
	assert.Equal(t, Clean("application/json", []byte("- a\n- b\n- a\n- b\n")), "- a\n- b\n- a\n- b")
	assert.Equal(t, Clean("text/html", []byte("<p>a</p>")), "a")
}

func TestCollapseListsLeavesCodeAlone(t *testing.T) {
	in := "- a\n- b\n```\n- a\n- b\n```\n- c"
	assert.Equal(t, collapseLists(in), in)
}

func TestPlainText(t *testing.T) {
	assert.Equal(t, PlainText([]byte("a  \r\nb\n\n\n\nc\xff")), "a\nb\n\nc�")
}

func FuzzCleanHTML(f *testing.F) {
	for _, s := range []string{page, guide, "<pre>a</pre>", "<script>", "<svg/><p>x", "&#0;&#xD800;", "<h1><li><td>",
		"\xff<p>\xfe", "<table><tr><th>a<td><table><tr><td>b", `<pre class="language-go"><code>x`, "<ul><ul><ul><li>x",
		`<p hidden><b>x`, "<main><header>h</header></main>"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out := CleanHTML(in)
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 out of %q", in)
		}
		// Markers, separators and entity expansion stay within a small factor.
		if len(out) > 4*len(in)+16 {
			t.Fatalf("%d bytes out of %d", len(out), len(in))
		}
		if strings.TrimSpace(out) != out {
			t.Fatalf("untrimmed output %q", out)
		}
	})
}
