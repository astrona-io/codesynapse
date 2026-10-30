package fetchsource

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds on pruning. MaxQuestion is the longest question fetch_source
// takes, in characters; at most maxTerms words of it are matched. A page
// with more than maxSections headings keeps the rest in its last section.
// Short code blocks of at most maxSignatureLines lines that declare
// something are kept from unmatched sections too, at most maxSignatures.
const (
	MaxQuestion       = 500
	maxTerms          = 16
	maxSections       = 4096
	maxPruneInput     = 4 * MaxBody
	maxSignatureLines = 3
	maxSignatures     = 32
)

// Pruned is the part of a page PruneMarkdown keeps.
type Pruned struct {
	Text string
	// Sections counts the page's headed sections; Kept those kept whole.
	Sections, Kept int
	// Pruned is true when sections were left out.
	Pruned bool
}

// section is a heading and the lines up to the next heading of any level.
// The preamble (the title and anything before the first heading) is
// section 0 with level 0.
type section struct {
	level         int
	lines         []string
	heading, code string // lower case, for matching
	body          string
}

// PruneMarkdown keeps the parts of a Markdown-like page that answer
// question: the preamble with the page title, every section whose heading
// or code names a word of the question (with its subsections and the
// headings above it), and short declaration blocks of the other sections.
// When no heading or code matches, sections whose prose names a word are
// kept instead; when nothing matches, or there is no question, the page is
// returned whole. Sections are kept or left out whole, never cut, so a code
// block is never split.
func PruneMarkdown(text, question string) Pruned {
	terms := Terms(question)
	if len(text) > maxPruneInput {
		cut := maxPruneInput
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	secs := sections(text)
	whole := Pruned{Text: text, Sections: len(secs) - 1, Kept: len(secs) - 1}
	if len(terms) == 0 || len(secs) < 2 {
		return whole
	}
	matched := match(secs, terms, func(s *section) string { return s.heading + "\n" + s.code })
	if len(matched) == 0 {
		matched = match(secs, terms, func(s *section) string { return s.body })
	}
	if len(matched) == 0 {
		return whole
	}

	const (
		drop = iota
		headingOnly
		signatures
		full
	)
	keep := make([]int, len(secs))
	keep[0] = full
	for _, i := range matched {
		keep[i] = full
		for j := i + 1; j < len(secs) && secs[j].level > secs[i].level; j++ {
			keep[j] = full
		}
		// The headings above it, so the agent sees where it sits.
		lvl := secs[i].level
		for j := i - 1; j > 0 && lvl > 1; j-- {
			if secs[j].level < lvl {
				keep[j] = max(keep[j], headingOnly)
				lvl = secs[j].level
			}
		}
	}
	sigs := 0
	var out []string
	kept := 0
	for i, s := range secs {
		switch keep[i] {
		case full:
			out = append(out, s.lines...)
			if i > 0 {
				kept++
			}
			continue
		case headingOnly:
			out = append(out, s.lines[0], "")
		}
		if sigs >= maxSignatures {
			continue
		}
		for _, b := range signatureBlocks(s.lines[1:]) {
			if sigs >= maxSignatures {
				break
			}
			if keep[i] == drop {
				out = append(out, s.lines[0], "")
				keep[i] = signatures
			}
			out = append(out, b...)
			out = append(out, "")
			sigs++
		}
	}
	return Pruned{Text: collapseLists(strings.Join(out, "\n")), Sections: len(secs) - 1, Kept: kept, Pruned: kept < len(secs)-1}
}

// match returns the sections (not the preamble) whose text names a term.
func match(secs []section, terms []string, text func(*section) string) []int {
	var out []int
	for i := 1; i < len(secs); i++ {
		t := text(&secs[i])
		if slices.ContainsFunc(terms, func(term string) bool { return containsWord(t, term) }) {
			out = append(out, i)
		}
	}
	return out
}

// sections splits text at ATX headings outside code fences.
func sections(text string) []section {
	secs := []section{{}}
	fence := false
	var code strings.Builder
	finish := func() {
		s := &secs[len(secs)-1]
		s.code = strings.ToLower(code.String())
		s.body = strings.ToLower(strings.Join(s.lines, "\n"))
		code.Reset()
	}
	for line := range strings.SplitSeq(text, "\n") {
		if isFence(line) {
			fence = !fence
		} else if fence {
			code.WriteString(line)
			code.WriteByte('\n')
		} else if lvl, h := heading(line); lvl > 0 && len(secs) <= maxSections {
			finish()
			secs = append(secs, section{level: lvl, heading: strings.ToLower(h)})
		} else {
			inlineCode(&code, line)
		}
		secs[len(secs)-1].lines = append(secs[len(secs)-1].lines, line)
	}
	finish()
	return secs
}

// heading returns the level and text of an ATX heading line ("## Title").
func heading(line string) (int, string) {
	n := 0
	for n < len(line) && n < 7 && line[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(line) || line[n] != ' ' {
		return 0, ""
	}
	return n, strings.TrimSpace(line[n+1:])
}

// inlineCode adds the `code` spans of line to b.
func inlineCode(b *strings.Builder, line string) {
	for {
		_, rest, ok := strings.Cut(line, "`")
		if !ok {
			return
		}
		span, after, ok := strings.Cut(rest, "`")
		if !ok {
			return
		}
		b.WriteString(span)
		b.WriteByte('\n')
		line = after
	}
}

// declPrefixes start a line that declares something in common languages.
var declPrefixes = []string{
	"func ", "type ", "def ", "async def ", "class ", "interface ", "struct ", "enum ", "trait ", "fn ", "pub fn ",
	"pub struct ", "pub enum ", "pub trait ", "impl ", "function ", "export ", "public ", "protected ", "static ",
	"const ", "let ", "var ", "val ", "module ", "package ",
}

// signatureBlocks returns the fenced blocks of lines that hold a short
// declaration: at most maxSignatureLines lines whose first one declares.
func signatureBlocks(lines []string) [][]string {
	var out [][]string
	for i := 0; i < len(lines); i++ {
		if !isFence(lines[i]) {
			continue
		}
		j := i + 1
		for j < len(lines) && !isFence(lines[j]) {
			j++
		}
		if j >= len(lines) {
			return out // unclosed: not a block to copy
		}
		body := lines[i+1 : j]
		if len(body) > 0 && len(body) <= maxSignatureLines {
			first := strings.TrimSpace(body[0])
			if slices.ContainsFunc(declPrefixes, func(p string) bool { return strings.HasPrefix(first, p) }) {
				out = append(out, lines[i:j+1])
			}
		}
		i = j
	}
	return out
}

// stopWords are left out of a question's terms: they match everywhere.
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "can": true,
	"do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "if": true, "in": true, "is": true,
	"it": true, "its": true, "me": true, "my": true, "of": true, "on": true, "or": true, "should": true, "the": true,
	"that": true, "this": true, "to": true, "use": true, "using": true, "what": true, "when": true, "where": true,
	"which": true, "who": true, "why": true, "will": true, "with": true, "you": true, "your": true, "we": true,
	"there": true, "into": true, "about": true, "between": true,
}

// Terms are the words of question that are matched: lower case, at least
// two characters, no stop words, at most maxTerms, in order.
func Terms(question string) []string {
	if utf8.RuneCountInString(question) > MaxQuestion {
		question = string([]rune(question)[:MaxQuestion])
	}
	var out []string
	for f := range strings.FieldsFuncSeq(strings.ToLower(question), func(r rune) bool { return !isWordRune(r) }) {
		if utf8.RuneCountInString(f) < 2 || stopWords[f] || slices.Contains(out, f) {
			continue
		}
		out = append(out, f)
		if len(out) == maxTerms {
			break
		}
	}
	return out
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// containsWord reports whether hay (lower case) names term at the start of
// a word. Terms of four or more characters may continue (client matches
// clients and clientconn); shorter ones must end the word, so "go" does not
// match "good".
func containsWord(hay, term string) bool {
	for off := 0; off <= len(hay)-len(term); {
		i := strings.Index(hay[off:], term)
		if i < 0 {
			return false
		}
		i += off
		before, _ := utf8.DecodeLastRuneInString(hay[:i])
		after, _ := utf8.DecodeRuneInString(hay[i+len(term):])
		if (i == 0 || !isWordRune(before)) && (len(term) >= 4 || i+len(term) == len(hay) || !isWordRune(after)) {
			return true
		}
		off = i + 1
	}
	return false
}
