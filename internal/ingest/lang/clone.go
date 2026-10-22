package lang

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/duplicates"
)

// The same thresholds as the Go extractor: below them a function is too small
// for sharing it to be worth the trouble.
const (
	minCloneLOC   = 6
	minCloneLines = 5
)

type cloneTok struct {
	kind byte // i identifier, k keyword, s string, n number, p punctuation, l logical line start
	text string
	lvl  int // indentation level of an 'l' token
}

var cloneKeywordsPy = setOf("False None True and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield self cls match case")

var cloneKeywordsJS = setOf("abstract as async await break case catch class const continue debugger default delete do else enum export extends false finally for from function get if implements import in instanceof interface let new null of private protected public readonly return set static super switch this throw true try typeof undefined var void while with yield")

func setOf(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// cloneHash fingerprints the structure of a function body with local names,
// string and number values abstracted, like the Go extractor does from the
// syntax tree. These parsers have no scopes, so an identifier counts as a
// name worth keeping only when it is called or read as a member; every other
// identifier is treated as a variable. That matches renamed copies and can
// also match code that differs only in what it reads, which the size
// thresholds keep rare. It returns "" for small functions and generated files.
func cloneHash(lang string, text []byte) (hash, sig string) {
	if generatedMarker(text) {
		return "", ""
	}
	py := lang == Python
	toks := cloneTokens(text, py)
	lines := 0
	for _, t := range toks {
		if t.kind == 'l' {
			lines++
		}
	}
	if !py {
		lines = codeLines(text)
	}
	if lines < minCloneLines {
		return "", ""
	}
	kw := cloneKeywordsJS
	if py {
		kw = cloneKeywordsPy
	}
	var out []string
	vars := map[string]int{}
	named := false // the function's own name is the first plain identifier
	base := -1
	for i, t := range toks {
		switch t.kind {
		case 'l':
			if base < 0 {
				base = t.lvl
			}
			out = append(out, fmt.Sprintf("L%d", t.lvl-base))
		case 'k', 'p':
			out = append(out, t.text)
		case 's':
			out = append(out, "S")
		case 'n':
			out = append(out, "N")
		case 'i':
			switch {
			case kw[t.text]:
				out = append(out, t.text)
			case !named:
				named = true
				out = append(out, "NAME")
			case i > 0 && toks[i-1].text == ".", i+1 < len(toks) && toks[i+1].text == "(":
				out = append(out, "id:"+t.text)
			default:
				n, ok := vars[t.text]
				if !ok {
					n = len(vars)
					vars[t.text] = n
				}
				out = append(out, fmt.Sprintf("v%d", n))
			}
		}
	}
	return duplicates.Hash(out), duplicates.Sig(out)
}

func generatedMarker(text []byte) bool {
	head := text[:min(len(text), 400)]
	h := bytes.ToLower(head)
	return bytes.Contains(h, []byte("@generated")) || bytes.Contains(h, []byte("code generated")) || bytes.Contains(h, []byte("do not edit"))
}

// codeLines counts lines with something other than a comment or braces.
func codeLines(text []byte) int {
	n := 0
	for _, line := range bytes.Split(text, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		if s == "" || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") || strings.HasPrefix(s, "/*") || s == "{" || s == "}" || s == "});" || s == "})" {
			continue
		}
		n++
	}
	return n
}

// cloneTokens splits code into tokens, dropping comments. For Python it emits
// a line-start token carrying the indentation level (blocks are indentation)
// and drops docstrings; for the brace languages whitespace carries no meaning.
func cloneTokens(src []byte, py bool) []cloneTok {
	var toks []cloneTok
	var indents []int
	depth, i := 0, 0
	atLineStart := true
	emit := func(k byte, s string) { toks = append(toks, cloneTok{kind: k, text: s}) }
	kw := cloneKeywordsJS
	if py {
		kw = cloneKeywordsPy
	}
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			atLineStart = depth == 0
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case py && c == '#' || !py && c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case !py && c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
		default:
			if atLineStart && py {
				col := lineIndent(src, i)
				for len(indents) > 0 && indents[len(indents)-1] > col {
					indents = indents[:len(indents)-1]
				}
				if len(indents) == 0 || indents[len(indents)-1] < col {
					indents = append(indents, col)
				}
				toks = append(toks, cloneTok{kind: 'l', lvl: len(indents)})
			}
			atLineStart = false
			switch {
			case c == '"' || c == '\'' || c == '`':
				i = skipString(src, i, py)
				emit('s', "")
			case c >= '0' && c <= '9':
				for i < len(src) && (isWord(src[i]) || src[i] == '.') {
					i++
				}
				emit('n', "")
			case isWord(c) && (c < '0' || c > '9'):
				j := i
				for j < len(src) && isWord(src[j]) {
					j++
				}
				w := string(src[i:j])
				// String prefixes (f"", r'', b"") belong to the string.
				if py && j < len(src) && (src[j] == '"' || src[j] == '\'') && len(w) <= 2 && strings.Trim(strings.ToLower(w), "rbfu") == "" {
					i = skipString(src, j, py)
					emit('s', "")
					continue
				}
				if kw[w] {
					emit('k', w)
				} else {
					emit('i', w)
				}
				i = j
			default:
				switch c {
				case '(', '[', '{':
					depth++
				case ')', ']', '}':
					depth = max(0, depth-1)
				}
				emit('p', string(c))
				i++
			}
		}
	}
	if py {
		toks = dropDocstrings(toks)
	}
	return toks
}

func isWord(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

func lineIndent(src []byte, at int) int {
	start := at
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	return at - start
}

// skipString returns the index after the string literal starting at i.
func skipString(src []byte, i int, py bool) int {
	q := src[i]
	if py && i+2 < len(src) && src[i+1] == q && src[i+2] == q {
		end := bytes.Index(src[i+3:], bytes.Repeat([]byte{q}, 3))
		if end < 0 {
			return len(src)
		}
		return i + 3 + end + 3
	}
	for i++; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case q:
			return i + 1
		case '\n':
			if q != '`' {
				return i
			}
		}
	}
	return len(src)
}

// dropDocstrings removes a string that is a whole statement (a docstring or a
// string used as a comment), so documented and undocumented copies match.
func dropDocstrings(toks []cloneTok) []cloneTok {
	out := toks[:0:0]
	for i, t := range toks {
		if t.kind == 's' && i > 0 && toks[i-1].kind == 'l' && (i+1 == len(toks) || toks[i+1].kind == 'l') {
			out = out[:len(out)-1] // and its line start
			continue
		}
		out = append(out, t)
	}
	return out
}
