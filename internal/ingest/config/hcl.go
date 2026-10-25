package config

import (
	"strings"
)

// A structural reader for HCL (Terraform), enough for rules: blocks with
// their labels and attributes with their line, literal strings and bools.
// Expressions are not evaluated; a value built from variables or functions
// is kept as raw text and never treated as a literal. Pure Go and bounded,
// like the other parsers here.

// hclBlock is a block: resource "aws_s3_bucket" "logs" { … }.
type hclBlock struct {
	Type   string
	Labels []string
	Line   int
	Attrs  map[string]hclAttr
	Blocks []*hclBlock
}

// hclAttr is one attribute. Strings are the string literals in its
// expression, in order; Literal is set when the whole value is one string
// without interpolation; Bool is "true" or "false" for a bare bool.
type hclAttr struct {
	Line    int
	Raw     string
	Strings []string
	Literal bool
	Bool    string
}

const (
	maxHCLDepth  = 32
	maxHCLBlocks = 20_000
)

type hclTokKind int

const (
	tokIdent hclTokKind = iota
	tokString
	tokLBrace
	tokRBrace
	tokEq
	tokNewline
	tokOther
)

type hclTok struct {
	kind hclTokKind
	text string // the string's content (escapes kept) or the token text
	line int
	// interp marks a string with ${…} or %{…}.
	interp bool
}

// lexHCL splits src into tokens; comments vanish, heredocs become strings.
func lexHCL(src string) []hclTok {
	var out []hclTok
	line := 1
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			out = append(out, hclTok{kind: tokNewline, line: line})
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#' || (c == '/' && i+1 < len(src) && src[i+1] == '/'):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				end = len(src) - i - 2
			}
			line += strings.Count(src[i:i+2+end], "\n")
			i += end + 4
		case c == '"':
			start, j, interp := line, i+1, false
			var b strings.Builder
			for j < len(src) && src[j] != '"' && src[j] != '\n' {
				switch {
				case src[j] == '\\' && j+1 < len(src):
					b.WriteByte(src[j])
					b.WriteByte(src[j+1])
					j += 2
					continue
				case (src[j] == '$' || src[j] == '%') && j+1 < len(src) && src[j+1] == '{':
					interp = true
					// Skip the template, which may hold quoted strings.
					depth := 0
					for j < len(src) && src[j] != '\n' {
						if src[j] == '{' {
							depth++
						} else if src[j] == '}' {
							depth--
							if depth == 0 {
								break
							}
						}
						b.WriteByte(src[j])
						j++
					}
				}
				if j < len(src) && src[j] != '\n' {
					b.WriteByte(src[j])
					j++
				}
			}
			out = append(out, hclTok{kind: tokString, text: b.String(), line: start, interp: interp})
			i = j + 1
		case c == '<' && strings.HasPrefix(src[i:], "<<"):
			// Heredoc: <<EOT or <<-EOT, up to a line that is the marker.
			j := i + 2
			if j < len(src) && src[j] == '-' {
				j++
			}
			k := j
			for k < len(src) && (isIdentByte(src[k])) {
				k++
			}
			marker := src[j:k]
			if marker == "" {
				out = append(out, hclTok{kind: tokOther, text: "<", line: line})
				i++
				continue
			}
			start := line
			rest := src[k:]
			body, end := "", len(src)
			for off := 0; off < len(rest); {
				nl := strings.IndexByte(rest[off:], '\n')
				if nl < 0 {
					break
				}
				off += nl + 1
				lineEnd := strings.IndexByte(rest[off:], '\n')
				if lineEnd < 0 {
					lineEnd = len(rest) - off
				}
				if strings.TrimSpace(rest[off:off+lineEnd]) == marker {
					body = rest[:off]
					end = k + off + lineEnd
					break
				}
			}
			line += strings.Count(src[i:end], "\n")
			out = append(out, hclTok{kind: tokString, text: body, line: start, interp: strings.Contains(body, "${")})
			i = end
		case c == '{':
			out = append(out, hclTok{kind: tokLBrace, text: "{", line: line})
			i++
		case c == '}':
			out = append(out, hclTok{kind: tokRBrace, text: "}", line: line})
			i++
		case c == '=' && isAssign(src, i):
			out = append(out, hclTok{kind: tokEq, text: "=", line: line})
			i++
		case isIdentByte(c):
			j := i
			for j < len(src) && (isIdentByte(src[j]) || src[j] == '.' || src[j] == '-') {
				j++
			}
			out = append(out, hclTok{kind: tokIdent, text: src[i:j], line: line})
			i = j
		default:
			out = append(out, hclTok{kind: tokOther, text: string(c), line: line})
			i++
		}
	}
	return out
}

// isAssign reports an "=" at i that assigns, not part of ==, =>, !=, <= or >=.
func isAssign(src string, i int) bool {
	if i+1 < len(src) && (src[i+1] == '=' || src[i+1] == '>') {
		return false
	}
	return i == 0 || !strings.ContainsRune("!<>=", rune(src[i-1]))
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// parseHCL returns the top-level blocks of src.
func parseHCL(src string) []*hclBlock {
	p := &hclParser{toks: lexHCL(src)}
	root := p.body(0)
	return root.Blocks
}

type hclParser struct {
	toks   []hclTok
	pos    int
	blocks int
}

func (p *hclParser) peek() *hclTok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

// body reads attributes and blocks up to the closing brace or the end.
func (p *hclParser) body(depth int) *hclBlock {
	b := &hclBlock{Attrs: map[string]hclAttr{}}
	for {
		t := p.peek()
		switch {
		case t == nil:
			return b
		case t.kind == tokNewline:
			p.pos++
		case t.kind == tokRBrace:
			p.pos++
			return b
		case t.kind == tokIdent && p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tokEq:
			name, line := t.text, t.line
			p.pos += 2
			b.Attrs[name] = p.expr(line)
		case t.kind == tokIdent || t.kind == tokString:
			blk := &hclBlock{Type: t.text, Line: t.line}
			p.pos++
			for q := p.peek(); q != nil && (q.kind == tokIdent || q.kind == tokString); q = p.peek() {
				blk.Labels = append(blk.Labels, q.text)
				p.pos++
			}
			if q := p.peek(); q == nil || q.kind != tokLBrace {
				p.skipLine()
				continue
			}
			p.pos++
			if depth >= maxHCLDepth || p.blocks >= maxHCLBlocks {
				p.skipBlock()
				continue
			}
			p.blocks++
			inner := p.body(depth + 1)
			blk.Attrs, blk.Blocks = inner.Attrs, inner.Blocks
			b.Blocks = append(b.Blocks, blk)
		default:
			p.skipLine()
		}
	}
}

// expr reads an attribute value up to the end of its line, following
// brackets and braces across lines.
func (p *hclParser) expr(line int) hclAttr {
	a := hclAttr{Line: line}
	var raw []string
	depth, n := 0, 0
	for t := p.peek(); t != nil; t = p.peek() {
		if depth == 0 && (t.kind == tokNewline || t.kind == tokRBrace) {
			break
		}
		p.pos++
		n++
		switch t.kind {
		case tokLBrace:
			depth++
		case tokRBrace:
			depth--
		case tokOther:
			switch t.text {
			case "[", "(":
				depth++
			case "]", ")":
				depth--
			}
		case tokString:
			a.Strings = append(a.Strings, t.text)
			raw = append(raw, `"`+t.text+`"`)
			continue
		case tokNewline:
			continue
		}
		raw = append(raw, t.text)
	}
	a.Raw = strings.Join(raw, " ")
	if n == 1 && len(a.Strings) == 1 && !p.toks[p.pos-1].interp {
		a.Literal = true
	}
	if n == 1 && (a.Raw == "true" || a.Raw == "false") {
		a.Bool = a.Raw
	}
	return a
}

func (p *hclParser) skipLine() {
	for t := p.peek(); t != nil && t.kind != tokNewline; t = p.peek() {
		if t.kind == tokLBrace {
			p.pos++
			p.skipBlock()
			continue
		}
		p.pos++
	}
}

func (p *hclParser) skipBlock() {
	depth := 1
	for t := p.peek(); t != nil && depth > 0; t = p.peek() {
		switch t.kind {
		case tokLBrace:
			depth++
		case tokRBrace:
			depth--
		}
		p.pos++
	}
}
