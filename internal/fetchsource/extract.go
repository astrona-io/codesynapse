package fetchsource

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Tier 2 (docs/plans/TEXT_OPTIMIZATION_PLAN.md, pillar B): when the cleaned
// and pruned page is still long, the bridge asks the server's local-only
// source-extract task for the parts that answer the question. The page and
// the question are sent for that one request and never stored; the answer
// is model output and is checked here before the agent sees it.

// Tier 2 limits.
const (
	// ExtractThreshold is the tier-1 text length (characters) from which
	// the bridge asks for an extract; shorter text is returned as it is.
	ExtractThreshold = 6000
	// MaxExtractInput bounds the page text sent (bytes). A longer page is
	// returned as tier 1 gave it: an extract of part of a page could miss
	// the answer and look complete.
	MaxExtractInput = 96 << 10
	// MaxExtract caps the extract (characters).
	MaxExtract = 8000
	// maxSnippets bounds the code spans and blocks checked in one answer.
	maxSnippets = 512
)

// NotInPage is what the model answers when the page does not answer the
// question; the bridge then returns the tier-1 text.
const NotInPage = "NOT IN THE PAGE"

// droppedCode replaces a code span the page does not contain.
const droppedCode = "[code not in the page, dropped]"

// ExtractRequest is the body of POST /api/v1/repos/{repo}/source-extract.
type ExtractRequest struct {
	Question string `json:"question"`
	Text     string `json:"text"`
}

// Validate checks the request against the limits.
func (r ExtractRequest) Validate() error {
	q := strings.TrimSpace(r.Question)
	switch {
	case q == "":
		return fmt.Errorf("%w: question is empty", ErrInvalid)
	case utf8.RuneCountInString(q) > MaxQuestion:
		return fmt.Errorf("%w: question is longer than %d characters", ErrInvalid, MaxQuestion)
	case strings.TrimSpace(r.Text) == "":
		return fmt.Errorf("%w: text is empty", ErrInvalid)
	case len(r.Text) > MaxExtractInput:
		return fmt.Errorf("%w: text is longer than %d bytes", ErrInvalid, MaxExtractInput)
	case !utf8.ValidString(r.Text) || !utf8.ValidString(q):
		return fmt.Errorf("%w: text is not valid UTF-8", ErrInvalid)
	}
	return nil
}

// Extract is the answer of source-extract.
type Extract struct {
	// Found is false when the model said the page does not answer the
	// question; Text is then empty.
	Found       bool   `json:"found"`
	Text        string `json:"text"`
	Model       string `json:"model,omitempty"`
	InputTokens int    `json:"input_tokens,omitempty"`
	// Dropped counts code the model wrote that the page does not contain.
	Dropped  int      `json:"dropped,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

// CheckSnippets drops every piece of code in answer that does not appear in
// sent: a local model that misquotes or invents code must not hand it to the
// agent as the page's. Code is what Markdown shows as code: fenced blocks
// (``` or ~~~), indented blocks (four spaces or a tab, more inside a list)
// and inline spans delimited by backtick runs of equal length (CommonMark).
// A block is kept only when each of its non-blank lines, trimmed, occurs in
// sent; an inline span likewise, line by line. <pre>, <code>, <kbd>, <samp>
// and <tt> tags outside code are removed, so HTML cannot mark text as code
// unchecked. Lines are checked one at a time: page lines put in a new order
// pass, and a short span passes whenever the page contains it.
//
// It returns the checked answer and how many snippets were dropped. Past
// maxSnippets the rest of the answer is cut, so the check stays bounded.
func CheckSnippets(answer, sent string) (string, int) {
	c := snippetCheck{sent: sent}
	lines := strings.Split(answer, "\n")
	listIndent := -1 // content column of the list the text is in, or -1
	prevBlank := true
	for i := 0; i < len(lines) && !c.full(); {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			c.out.WriteString(line + "\n")
			prevBlank = true
			i++
			continue
		}
		ind := indentOf(line)
		if prevBlank && listIndent >= 0 && ind < listIndent && listItem(line) < 0 {
			listIndent = -1
		}
		codeAt := 4
		if listIndent >= 0 {
			codeAt = listIndent + 4
		}
		switch {
		case prevBlank && ind >= codeAt:
			i = c.indented(lines, i, codeAt)
		case fenceOf(line) != "":
			i = c.fenced(lines, i)
		default:
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) != "" && fenceOf(lines[j]) == "" && !startsBlock(lines[j]) {
				j++
			}
			if k := listItem(line); k >= 0 {
				listIndent = k
			}
			c.out.WriteString(c.inline(strings.Join(lines[i:j], "\n")) + "\n")
			i = j
		}
		prevBlank = false
	}
	return strings.TrimRight(c.out.String(), "\n"), c.drop
}

type snippetCheck struct {
	sent    string
	out     strings.Builder
	n, drop int
}

func (c *snippetCheck) full() bool { return c.n > maxSnippets }

// inPage reports whether every non-blank line of code, trimmed, is in sent.
func (c *snippetCheck) inPage(code string) bool {
	for l := range strings.SplitSeq(code, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.Contains(c.sent, t) {
			return false
		}
	}
	return true
}

// block writes lines when code is in the page and the drop marker otherwise.
func (c *snippetCheck) block(lines []string, code []string) {
	c.n++
	if c.full() {
		return
	}
	if c.inPage(strings.Join(code, "\n")) {
		for _, l := range lines {
			c.out.WriteString(l + "\n")
		}
		return
	}
	c.out.WriteString(droppedCode + "\n")
	c.drop++
}

// fenced checks the fenced block opening at lines[i] and returns the index
// after it. An unclosed fence runs to the end of the answer.
func (c *snippetCheck) fenced(lines []string, i int) int {
	fence := fenceOf(lines[i])
	j := i + 1
	for j < len(lines) && !closesFence(lines[j], fence) {
		j++
	}
	c.block(lines[i:min(j+1, len(lines))], lines[i+1:min(j, len(lines))])
	return j + 1
}

// indented checks the indented code block starting at lines[i]: lines
// indented at least codeAt columns, with blank lines between them.
func (c *snippetCheck) indented(lines []string, i, codeAt int) int {
	j, end := i, i
	for j < len(lines) {
		if strings.TrimSpace(lines[j]) == "" {
			j++
			continue
		}
		if indentOf(lines[j]) < codeAt {
			break
		}
		j++
		end = j
	}
	c.block(lines[i:end], lines[i:end])
	return end
}

// inline checks the code spans of one paragraph. A run of N backticks opens
// a span closed by the next run of exactly N; a run with no such partner is
// literal text.
func (c *snippetCheck) inline(s string) string {
	var b strings.Builder
	prose := 0
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
		b.WriteString(stripCodeTags(s[prose:p]))
		c.n++
		if c.inPage(s[q:end]) {
			b.WriteString(s[p : end+q-p])
		} else {
			b.WriteString(droppedCode)
			c.drop++
		}
		p = end + q - p
		prose = p
		if c.full() {
			return b.String()
		}
	}
	b.WriteString(stripCodeTags(s[prose:]))
	return b.String()
}

// closingRun is the start of the first run of exactly n backticks at or
// after from, or -1.
func closingRun(s string, from, n int) int {
	for p := from; p < len(s); {
		if s[p] != '`' {
			p++
			continue
		}
		q := p
		for q < len(s) && s[q] == '`' {
			q++
		}
		if q-p == n {
			return p
		}
		p = q
	}
	return -1
}

// codeTags are the HTML tags that show text as code. A removed tag becomes a
// space, so what is left cannot join into a new tag.
var codeTags = regexp.MustCompile(`(?i)</?(?:pre|code|kbd|samp|tt)\b[^>]*>`)

func stripCodeTags(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	return codeTags.ReplaceAllString(s, " ")
}

// fenceOf is the fence a line opens (three or more backticks or tildes), or
// "". A backtick fence's info string has no backticks.
func fenceOf(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	k := 0
	for k < len(t) && t[k] == t[0] {
		k++
	}
	if k < 3 || (t[0] == '`' && strings.Contains(t[k:], "`")) {
		return ""
	}
	return t[:k]
}

// closesFence reports whether line closes fence: only the fence character,
// at least as many as opened it.
func closesFence(line, fence string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= len(fence) && strings.Trim(t, fence[:1]) == ""
}

// indentOf is the leading whitespace of line in columns (tabs stop every 4).
func indentOf(line string) int {
	w := 0
	for _, r := range line {
		switch r {
		case ' ':
			w++
		case '\t':
			w += 4 - w%4
		default:
			return w
		}
	}
	return w
}

// listItem is the content column of the list item line starts, or -1. It is
// the smallest the item allows, so indented code is never missed.
func listItem(line string) int {
	ind := indentOf(line)
	t := strings.TrimLeft(line, " \t")
	if len(t) >= 2 && strings.IndexByte("-*+", t[0]) >= 0 && (t[1] == ' ' || t[1] == '\t') {
		return ind + 2
	}
	k := 0
	for k < len(t) && k < 9 && t[k] >= '0' && t[k] <= '9' {
		k++
	}
	if k > 0 && k+1 < len(t) && (t[k] == '.' || t[k] == ')') && (t[k+1] == ' ' || t[k+1] == '\t') {
		return ind + k + 2
	}
	return -1
}

// startsBlock reports whether line starts a list item or heading, which ends
// a paragraph (code spans do not cross it).
func startsBlock(line string) bool {
	return listItem(line) >= 0 || strings.HasPrefix(strings.TrimLeft(line, " \t"), "#")
}
