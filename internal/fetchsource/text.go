package fetchsource

import (
	"bytes"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipped elements hold no text a reader sees.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true, atom.Canvas: true,
}

// blocks start on a line of their own.
var blocks = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true, atom.Body: true,
	atom.Br: true, atom.Caption: true, atom.Dd: true, atom.Details: true, atom.Div: true, atom.Dl: true,
	atom.Dt: true, atom.Fieldset: true, atom.Figcaption: true, atom.Figure: true, atom.Footer: true,
	atom.Form: true, atom.Header: true, atom.Hr: true, atom.Main: true, atom.Nav: true, atom.Ol: true,
	atom.P: true, atom.Section: true, atom.Summary: true, atom.Table: true, atom.Tbody: true,
	atom.Thead: true, atom.Title: true, atom.Tr: true, atom.Ul: true,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

// HTMLText turns an HTML page into plain text: scripts, styles and other
// invisible elements are dropped, block elements start new lines, headings
// and list items keep a Markdown marker, and pre blocks keep their layout in
// a code fence. It reads tokens in one pass, so its work is linear in the
// input. Choosing which sections to keep is left to a later reduction step.
func HTMLText(src []byte) string {
	z := html.NewTokenizer(bytes.NewReader(src))
	var w textWriter
	skip, pre := 0, 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken: // io.EOF or a tokenizer limit; either way, done
			return w.String()
		case html.TextToken:
			if skip == 0 {
				w.text(string(z.Text()), pre > 0)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			if skipped[a] {
				if tt == html.StartTagToken {
					skip++
				}
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case a == atom.Pre:
				w.fence()
				if tt == html.StartTagToken {
					pre++
				}
			case a == atom.Li:
				w.line("- ")
			case headings[a] != "":
				w.blank()
				w.line(headings[a])
			case a == atom.Td || a == atom.Th:
				w.cell()
			case blocks[a]:
				w.line("")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			a := atom.Lookup(name)
			if skipped[a] {
				skip = max(skip-1, 0)
				continue
			}
			if skip > 0 {
				continue
			}
			switch {
			case a == atom.Pre && pre > 0:
				pre--
				w.fence()
			case headings[a] != "":
				w.blank()
			case blocks[a] || a == atom.Li:
				w.line("")
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
	w.space = false
	w.atLine = false
}

func (w *textWriter) fence() {
	w.line("```")
	w.b.WriteByte('\n')
}

var (
	trailingSpace = regexp.MustCompile(`(?m)[ \t]+$`)
	manyBlank     = regexp.MustCompile(`\n{3,}`)
)

func (w *textWriter) String() string {
	s := strings.ToValidUTF8(w.b.String(), "�")
	s = trailingSpace.ReplaceAllString(s, "")
	s = manyBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// PlainText tidies text that is not HTML: line endings, invalid UTF-8 and
// runs of blank lines.
func PlainText(src []byte) string {
	s := strings.ToValidUTF8(string(src), "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = trailingSpace.ReplaceAllString(s, "")
	s = manyBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
