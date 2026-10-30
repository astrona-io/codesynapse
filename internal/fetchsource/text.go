package fetchsource

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// cleanerVersion changes whenever CleanHTML keeps different text; cache
// entries made by another version are fetched again.
const cleanerVersion = 2

// Bounds on cleaning. The fetch caps the body at MaxBody already; the
// input cap holds for direct callers too. maxDepth stops the walk of a
// pathologically nested page (deeper content is dropped) and maxIndent the
// indentation of nested lists.
const (
	maxCleanInput = MaxBody
	maxDepth      = 256
	maxIndent     = 4
	maxLangLen    = 20
)

// dropped elements hold nothing a reader of the documentation needs:
// invisible content, site chrome and forms.
var dropped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Canvas: true,
	atom.Nav: true, atom.Aside: true, atom.Footer: true, atom.Form: true, atom.Button: true,
	atom.Select: true, atom.Textarea: true, atom.Input: true, atom.Dialog: true, atom.Img: true,
	atom.Picture: true, atom.Video: true, atom.Audio: true,
}

// droppedRoles mark site chrome on generic elements.
var droppedRoles = map[string]bool{
	"navigation": true, "contentinfo": true, "search": true, "menu": true, "menubar": true, "toolbar": true, "dialog": true,
}

// blocks start on a line of their own.
var blocks = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Blockquote: true, atom.Body: true,
	atom.Caption: true, atom.Dd: true, atom.Details: true, atom.Div: true, atom.Dl: true,
	atom.Dt: true, atom.Fieldset: true, atom.Figcaption: true, atom.Figure: true, atom.Header: true,
	atom.Hr: true, atom.Main: true, atom.Ol: true, atom.P: true, atom.Section: true, atom.Summary: true,
	atom.Table: true, atom.Tbody: true, atom.Thead: true, atom.Tfoot: true, atom.Title: true, atom.Ul: true,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

// CleanHTML turns an HTML page into Markdown-like text. The page is parsed
// with golang.org/x/net/html (the HTML5 algorithm, so broken markup is
// repaired as a browser would) and walked once:
//
//   - scripts, styles, SVG, forms, images (tracking pixels included), nav,
//     aside, footer and other site chrome are dropped, and so are elements
//     a browser does not show (the hidden attribute, aria-hidden="true",
//     display:none or visibility:hidden in the style attribute);
//   - a header is dropped unless it sits inside main or article, where it
//     usually holds the page's own title;
//   - headings keep a # marker, list items "- ", tables become | rows with
//     a separator after a header row, pre blocks a code fence (with the
//     language a language-* class names) and inline code backticks;
//   - whitespace is collapsed outside pre blocks, and a list repeated
//     verbatim (a menu printed twice) is kept once.
//
// Choosing which sections to keep is PruneMarkdown's job.
func CleanHTML(src []byte) string {
	if len(src) > maxCleanInput {
		src = src[:maxCleanInput]
	}
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil { // nested deeper than the parser takes (512 elements)
		return flatText(src)
	}
	var k walker
	k.walk(doc, 0)
	return collapseLists(k.w.String())
}

// walker carries the state of one CleanHTML walk.
type walker struct {
	w                    textWriter
	pre, main, cell, lst int
}

func (k *walker) walk(n *html.Node, depth int) {
	if depth > maxDepth {
		return
	}
	switch n.Type {
	case html.TextNode:
		k.w.text(n.Data, k.pre > 0)
	case html.DocumentNode:
		k.children(n, depth)
	case html.ElementNode:
		k.element(n, depth)
	}
}

func (k *walker) children(n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		k.walk(c, depth+1)
	}
}

func (k *walker) element(n *html.Node, depth int) {
	a := n.DataAtom
	if dropped[a] || hidden(n) || (a == atom.Header || attr(n, "role") == "banner") && k.main == 0 {
		return
	}
	switch {
	case a == atom.Pre:
		if k.cell > 0 { // a code block cannot live in a table row
			k.inline(textContent(n, depth))
			return
		}
		k.w.fence(lang(n))
		k.pre++
		k.children(n, depth)
		k.pre--
		k.w.fence("")
	case a == atom.Code && k.pre == 0:
		k.inline(textContent(n, depth))
	case a == atom.Br:
		k.block()
	case headings[a] != "":
		if k.cell > 0 {
			k.children(n, depth)
			return
		}
		k.w.blank()
		k.w.line(headings[a])
		k.children(n, depth)
		k.w.blank()
	case a == atom.Ul || a == atom.Ol:
		k.block()
		k.lst++
		k.children(n, depth)
		k.lst--
		k.block()
	case a == atom.Li:
		if k.cell > 0 {
			k.w.space = true
			k.children(n, depth)
			return
		}
		k.w.line(strings.Repeat("  ", min(max(k.lst-1, 0), maxIndent)) + "- ")
		k.children(n, depth)
		k.w.line("")
	case a == atom.Tr:
		k.row(n, depth)
	case a == atom.Main || a == atom.Article:
		k.block()
		k.main++
		k.children(n, depth)
		k.main--
		k.block()
	case blocks[a]:
		k.block()
		k.children(n, depth)
		k.block()
	default:
		k.children(n, depth)
	}
}

// block ends the line, or inside a table cell (which must stay on its row)
// leaves a space.
func (k *walker) block() {
	if k.cell > 0 {
		k.w.space = true
		return
	}
	k.w.line("")
}

// row writes a table row as "| a | b |" and, after a row of header cells
// only, a "| --- | --- |" separator.
func (k *walker) row(n *html.Node, depth int) {
	if k.cell > 0 { // a table inside a cell: flatten its rows
		k.children(n, depth)
		return
	}
	cells, heads := 0, 0
	k.w.line("")
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom != atom.Td && c.DataAtom != atom.Th || hidden(c) {
			continue
		}
		cells++
		if c.DataAtom == atom.Th {
			heads++
		}
		k.w.cell()
		k.cell++
		k.children(c, depth+1)
		k.cell--
	}
	if cells == 0 {
		return
	}
	k.w.raw(" |")
	if heads == cells {
		k.w.line("")
		k.w.raw("|" + strings.Repeat(" --- |", cells))
	}
	k.w.line("")
}

// inline writes code as `code`, or bare when it holds a backtick or is
// empty.
func (k *walker) inline(code string) {
	code = strings.Join(strings.Fields(code), " ")
	if code == "" {
		return
	}
	if strings.Contains(code, "`") {
		k.w.text(code, false)
		return
	}
	k.w.flushSpace()
	k.w.raw("`" + code + "`")
}

// hidden reports whether a browser would not show n.
func hidden(n *html.Node) bool {
	for _, at := range n.Attr {
		switch strings.ToLower(at.Key) {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(at.Val), "true") {
				return true
			}
		case "style":
			s := strings.ToLower(strings.Join(strings.Fields(at.Val), ""))
			if strings.Contains(s, "display:none") || strings.Contains(s, "visibility:hidden") {
				return true
			}
		case "role":
			if droppedRoles[strings.ToLower(strings.TrimSpace(at.Val))] {
				return true
			}
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, at := range n.Attr {
		if strings.EqualFold(at.Key, key) {
			return strings.ToLower(strings.TrimSpace(at.Val))
		}
	}
	return ""
}

// lang is the language a pre block or its code child names with a
// language-* or lang-* class, limited to a short safe word.
func lang(n *html.Node) string {
	classes := attr(n, "class")
	if c := n.FirstChild; c != nil && c.Type == html.ElementNode && c.DataAtom == atom.Code {
		classes += " " + attr(c, "class")
	}
	for _, c := range strings.Fields(classes) {
		for _, p := range []string{"language-", "lang-"} {
			if l, ok := strings.CutPrefix(c, p); ok && l != "" && len(l) <= maxLangLen && strings.IndexFunc(l, notLangRune) < 0 {
				return l
			}
		}
	}
	return ""
}

func notLangRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("+#-_", r)
}

// textContent is the visible text below n, within the depth cap.
func textContent(n *html.Node, depth int) string {
	var b strings.Builder
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		if depth > maxDepth {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch {
			case c.Type == html.TextNode:
				b.WriteString(c.Data)
			case c.Type == html.ElementNode && !dropped[c.DataAtom] && !hidden(c):
				walk(c, depth+1)
			}
		}
	}
	walk(n, depth)
	return b.String()
}

// flatText is the fallback for a page the parser refuses: its text in one
// pass of the tokenizer, without the content of raw-text and dropped
// elements and without any structure.
func flatText(src []byte) string {
	z := html.NewTokenizer(bytes.NewReader(src))
	var w textWriter
	skip := 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken: // io.EOF or a tokenizer limit; either way, done
			return collapseLists(w.String())
		case html.TextToken:
			if skip == 0 {
				w.text(string(z.Text()), false)
			}
		case html.StartTagToken, html.EndTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			switch {
			case !dropped[a] || a == atom.Img || a == atom.Input || a == atom.Embed: // void elements never close
				w.space = true
			case tt == html.StartTagToken:
				skip++
			default:
				skip = max(skip-1, 0)
			}
		}
	}
}

// textWriter builds the text, collapsing whitespace outside pre blocks.
type textWriter struct {
	b strings.Builder
	// space is a pending separator; atLine is true at the start of a line.
	space, atLine bool
}

func (w *textWriter) text(s string, pre bool) {
	if pre {
		w.flushSpace()
		w.b.WriteString(s)
		w.atLine = strings.HasSuffix(s, "\n")
		return
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			w.space = !w.atLine
			continue
		}
		w.flushSpace()
		w.b.WriteRune(r)
		w.atLine = false
	}
}

// raw writes s as it is, on the current line.
func (w *textWriter) raw(s string) {
	w.b.WriteString(s)
	w.space = false
	w.atLine = false
}

func (w *textWriter) flushSpace() {
	if w.space && !w.atLine {
		w.b.WriteByte(' ')
	}
	w.space = false
}

// line ends the current line (if any) and starts one with prefix.
func (w *textWriter) line(prefix string) {
	if !w.atLine && w.b.Len() > 0 {
		w.b.WriteByte('\n')
	}
	w.space = false
	w.atLine = true
	if prefix != "" {
		w.b.WriteString(prefix)
	}
}

// blank leaves an empty line before what comes next.
func (w *textWriter) blank() {
	w.line("")
	w.b.WriteByte('\n')
}

// cell separates table cells.
func (w *textWriter) cell() {
	if w.atLine {
		w.b.WriteString("| ")
	} else {
		w.b.WriteString(" | ")
	}
	// The cell's content joins the marker without a space of its own.
	w.space = false
	w.atLine = true
}

func (w *textWriter) fence(lang string) {
	w.line("```" + lang)
	w.b.WriteByte('\n')
	w.atLine = true
}

var (
	trailingSpace = regexp.MustCompile(`(?m)[ \t]+$`)
	manyBlank     = regexp.MustCompile(`\n{3,}`)
)

func (w *textWriter) String() string {
	return tidy(w.b.String())
}

func tidy(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = trailingSpace.ReplaceAllString(s, "")
	s = manyBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// PlainText tidies text that is not HTML: line endings, invalid UTF-8 and
// runs of blank lines.
func PlainText(src []byte) string {
	return tidy(strings.ReplaceAll(string(src), "\r\n", "\n"))
}

// Clean is the cleaned text of a body of media type mt: HTML through
// CleanHTML, Markdown and plain text (often Markdown served raw) tidied with
// repeated lists collapsed, anything else (JSON) only tidied.
func Clean(mt string, body []byte) string {
	switch mt {
	case "text/html":
		return CleanHTML(body)
	case "text/markdown", "text/plain":
		return collapseLists(PlainText(body))
	}
	return PlainText(body)
}

// collapseLists drops a run of list items that repeats an earlier run
// verbatim (a menu printed in the header and again in the footer). Code
// fences are left alone, and single items are never dropped.
func collapseLists(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	seen := map[string]bool{}
	fence := false
	for i := 0; i < len(lines); {
		if isFence(lines[i]) {
			fence = !fence
		}
		if fence || !isItem(lines[i]) {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i
		for j < len(lines) && isItem(lines[j]) {
			j++
		}
		run := strings.Join(lines[i:j], "\n")
		if j-i < 2 || !seen[run] {
			seen[run] = true
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return tidy(strings.Join(out, "\n"))
}

func isItem(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || t == "-"
}

func isFence(line string) bool {
	t := strings.TrimLeft(line, " ")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}
