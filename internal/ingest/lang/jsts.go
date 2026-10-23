package lang

import (
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

var (
	jsFunc    = regexp.MustCompile(`^\s*(export\s+(default\s+)?)?(async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*(<[^>]*>)?\s*\(`)
	jsArrow   = regexp.MustCompile(`^\s*(export\s+)?(const|let|var)\s+([A-Za-z_$][\w$]*)\s*(:[^=]+)?=\s*(async\s+)?(function\b|\([^)]*\)\s*(:\s*[^=]+)?=>|[A-Za-z_$][\w$]*\s*=>|<[^>]*>\s*\()`)
	jsClass   = regexp.MustCompile(`^\s*(export\s+(default\s+)?)?(abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsMethod  = regexp.MustCompile(`^\s*((?:public|private|protected|static|async|get|set|readonly|override|abstract)\s+)*\*?\s*(#?[A-Za-z_$][\w$]*)\s*(<[^>]*>)?\s*\(`)
	jsCall    = regexp.MustCompile(`(?:([A-Za-z_$][\w$]*)\s*\??\.\s*)?([A-Za-z_$][\w$]*)\s*(?:<[^<>()]*>)?\s*\(`)
	jsImport  = regexp.MustCompile(`^\s*import\s+(type\s+)?(.*?)\s*from\s*['"]([^'"]+)['"]`)
	jsBareImp = regexp.MustCompile(`^\s*import\s*['"]([^'"]+)['"]`)
	jsReexp   = regexp.MustCompile(`^\s*export\s+(?:\*|\{[^}]*\})\s*(?:as\s+\w+\s*)?from\s*['"]([^'"]+)['"]`)
	jsRequire = regexp.MustCompile(`(?:const|let|var)\s+(\{[^}]*\}|[A-Za-z_$][\w$]*)\s*=\s*require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsDynImp  = regexp.MustCompile(`\bimport\(\s*['"]([^'"]+)['"]\s*\)`)
	jsKeyword = map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true, "function": true, "return": true,
		"typeof": true, "new": true, "await": true, "super": true, "import": true, "require": true, "else": true,
		"do": true, "with": true, "void": true, "delete": true, "in": true, "of": true, "instanceof": true, "yield": true,
	}
	jsBranches = []string{"if", "for", "while", "case", "catch"}
)

// maskJS blanks out string, template and regex literal contents and
// comments, keeping line structure and ${...} expressions.
func maskJS(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	type mode int
	const (
		code mode = iota
		tmpl
	)
	var stack []mode // template nesting: inside ${ } we are back in code
	braceDepth := []int{}
	prevSig := byte('(') // previous significant char, for regex detection
	for i := 0; i < len(b); i++ {
		c := b[i]
		inTmpl := len(stack) > 0 && stack[len(stack)-1] == tmpl
		if inTmpl {
			switch {
			case c == '\\':
				blank(i)
				if i+1 < len(b) {
					i++
					blank(i)
				}
			case c == '`':
				stack = stack[:len(stack)-1]
				prevSig = '`'
			case c == '$' && i+1 < len(b) && b[i+1] == '{':
				stack = append(stack, code)
				braceDepth = append(braceDepth, 0)
				i++
			default:
				blank(i)
			}
			continue
		}
		switch {
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				blank(i)
				i++
			}
			i--
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			for i < len(b) && (b[i] != '*' || i+1 >= len(b) || b[i+1] != '/') {
				blank(i)
				i++
			}
			if i+1 < len(b) {
				blank(i)
				blank(i + 1)
				i++
			}
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(b) && b[j] != c && b[j] != '\n' {
				if b[j] == '\\' {
					blank(j)
					j++
				}
				if j < len(b) {
					blank(j)
				}
				j++
			}
			i = j
			prevSig = c
		case c == '`':
			stack = append(stack, tmpl)
		case c == '/' && strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prevSig) >= 0:
			// Regex literal.
			j, inClass := i+1, false
			for j < len(b) && b[j] != '\n' {
				if b[j] == '\\' {
					blank(j)
					j++
				} else if b[j] == '[' {
					inClass = true
				} else if b[j] == ']' {
					inClass = false
				} else if b[j] == '/' && !inClass {
					break
				}
				if j < len(b) {
					blank(j)
				}
				j++
			}
			i = j
			prevSig = '/'
		default:
			if len(stack) > 0 && stack[len(stack)-1] == code {
				d := &braceDepth[len(braceDepth)-1]
				if c == '{' {
					*d++
				} else if c == '}' {
					if *d == 0 {
						stack = stack[:len(stack)-1]
						braceDepth = braceDepth[:len(braceDepth)-1]
						continue
					}
					*d--
				}
			}
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prevSig = c
				if isIdent(rune(c)) {
					prevSig = 'a'
				}
			}
		}
	}
	return string(out)
}

// depths returns the brace depth at the start of every line of masked.
func depths(masked string) []int {
	lines := strings.Split(masked, "\n")
	out := make([]int, len(lines))
	d := 0
	for i, l := range lines {
		out[i] = d
		d += strings.Count(l, "{") - strings.Count(l, "}")
		if d < 0 {
			d = 0
		}
	}
	return out
}

// blockEnd finds the line where the block opened at or after line start
// (1-based) closes. Arrow functions without a body end on their statement.
func blockEnd(lines []string, start int) int {
	d, opened := 0, false
	for i := start - 1; i < len(lines) && i < start+2000; i++ {
		for _, c := range lines[i] {
			switch c {
			case '{':
				d++
				opened = true
			case '}':
				d--
				if opened && d == 0 {
					return i + 1
				}
			}
		}
		if !opened && i >= start-1 && (strings.HasSuffix(strings.TrimSpace(lines[i]), ";") || i > start+3) {
			return i + 1
		}
	}
	return start
}

func parseJS(src []byte, _ bool) *fileResult {
	raw := strings.Split(string(src), "\n")
	masked := maskJS(string(src))
	lines := strings.Split(masked, "\n")
	depth := depths(masked)
	fr := &fileResult{}

	type classCtx struct {
		name  string
		depth int
		end   int
	}
	var classes []classCtx
	for i, l := range lines {
		n := i + 1
		for len(classes) > 0 && n > classes[len(classes)-1].end {
			classes = classes[:len(classes)-1]
		}
		// Imports (top level only).
		if depth[i] == 0 {
			if m := jsImport.FindStringSubmatch(raw[i]); m != nil {
				fr.imports = append(fr.imports, jsImportNames(m[2], m[3], n))
				continue
			}
			if m := jsBareImp.FindStringSubmatch(raw[i]); m != nil {
				fr.imports = append(fr.imports, importRef{spec: m[1], names: map[string]string{}, line: n})
				continue
			}
			if m := jsReexp.FindStringSubmatch(raw[i]); m != nil {
				fr.imports = append(fr.imports, importRef{spec: m[1], names: map[string]string{}, line: n})
			}
		}
		for _, m := range jsRequire.FindAllStringSubmatch(raw[i], -1) {
			fr.imports = append(fr.imports, requireNames(m[1], m[2], n))
		}
		for _, m := range jsDynImp.FindAllStringSubmatch(raw[i], -1) {
			fr.imports = append(fr.imports, importRef{spec: m[1], names: map[string]string{}, line: n})
		}

		if depth[i] == 0 {
			if m := jsClass.FindStringSubmatch(l); m != nil {
				end := blockEnd(lines, n)
				fr.symbols = append(fr.symbols, &symbol{
					name: m[4], kind: model.KindType, start: n, end: end, signature: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw[i]), "{")),
					exported: m[1] != "", doc: jsDoc(raw, i),
				})
				classes = append(classes, classCtx{name: m[4], depth: depth[i] + 1, end: end})
				continue
			}
			if m := jsFunc.FindStringSubmatch(l); m != nil {
				fr.symbols = append(fr.symbols, &symbol{
					name: m[4], kind: model.KindFunction, start: n, end: blockEnd(lines, n),
					signature: jsSignature(raw, i), exported: m[1] != "", doc: jsDoc(raw, i), root: m[2] != "",
				})
				continue
			}
			if m := jsArrow.FindStringSubmatch(l); m != nil {
				fr.symbols = append(fr.symbols, &symbol{
					name: m[3], kind: model.KindFunction, start: n, end: blockEnd(lines, n),
					signature: jsSignature(raw, i), exported: m[1] != "", doc: jsDoc(raw, i),
				})
				continue
			}
		}
		if len(classes) > 0 && depth[i] == classes[len(classes)-1].depth {
			if m := jsMethod.FindStringSubmatch(l); m != nil && !jsKeyword[m[2]] && strings.Contains(l, ")") || m != nil && strings.HasSuffix(strings.TrimSpace(l), "(") {
				if !jsKeyword[m[2]] {
					cls := classes[len(classes)-1].name
					name := strings.TrimPrefix(m[2], "#")
					fr.symbols = append(fr.symbols, &symbol{
						name: cls + "." + name, kind: model.KindMethod, class: cls, start: n, end: blockEnd(lines, n),
						signature: jsSignature(raw, i), exported: !strings.HasPrefix(m[2], "#") && !strings.Contains(l, "private "), doc: jsDoc(raw, i),
					})
				}
			}
		}
	}

	// Attribute calls and findings to the innermost symbol containing the
	// line. Filling a table once replaces a scan of every symbol per line.
	owners := make([]*symbol, len(lines)+1)
	for _, s := range fr.symbols {
		if s.kind == model.KindType {
			continue
		}
		for line := max(s.start, 1); line <= min(s.end, len(lines)); line++ {
			if best := owners[line]; best == nil || s.start >= best.start {
				owners[line] = s
			}
		}
	}
	childProcess := false
	for _, imp := range fr.imports {
		if imp.spec == "child_process" || imp.spec == "node:child_process" {
			childProcess = true
		}
	}
	for i, l := range lines {
		n := i + 1
		o := owners[n]
		for _, m := range jsCall.FindAllStringSubmatch(l, -1) {
			if jsKeyword[m[2]] || (m[1] == "" && isDeclLine(l, m[2])) {
				continue
			}
			c := call{recv: m[1], name: m[2], line: n}
			if o != nil {
				o.calls = append(o.calls, c)
			} else {
				fr.topCalls = append(fr.topCalls, c)
			}
		}
		fr.findings = append(fr.findings, jsRules(raw[i], l, n, ownerName(o), childProcess)...)
	}
	for _, s := range fr.symbols {
		if s.kind == model.KindType {
			continue
		}
		body := strings.Join(lines[max(0, s.start-1):min(len(lines), s.end)], "\n")
		s.complexity = countBranches(body, jsBranches) + strings.Count(body, "?.")*0 + strings.Count(body, " ? ")
	}
	return fr
}

func isDeclLine(l, name string) bool {
	return strings.Contains(l, "function "+name) || strings.Contains(l, "function* "+name)
}

func jsImportNames(clause, spec string, line int) importRef {
	ref := importRef{spec: spec, names: map[string]string{}, line: line}
	clause = strings.TrimSpace(clause)
	if strings.HasPrefix(clause, "* as ") {
		ref.names[strings.TrimSpace(strings.TrimPrefix(clause, "* as "))] = "*"
		return ref
	}
	def, rest, hasBrace := strings.Cut(clause, "{")
	def = strings.Trim(strings.TrimSpace(def), ",")
	if def != "" {
		if strings.HasPrefix(def, "* as ") {
			ref.names[strings.TrimSpace(strings.TrimPrefix(def, "* as "))] = "*"
		} else {
			ref.names[def] = "default"
		}
	}
	if hasBrace {
		inner, _, _ := strings.Cut(rest, "}")
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "type "))
			member, alias, ok := strings.Cut(part, " as ")
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			if !ok {
				alias = member
			}
			ref.names[strings.TrimSpace(alias)] = member
		}
	}
	return ref
}

func requireNames(lhs, spec string, line int) importRef {
	ref := importRef{spec: spec, names: map[string]string{}, line: line}
	if strings.HasPrefix(lhs, "{") {
		for _, part := range strings.Split(strings.Trim(lhs, "{} "), ",") {
			member, alias, ok := strings.Cut(strings.TrimSpace(part), ":")
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			if !ok {
				alias = member
			}
			ref.names[strings.TrimSpace(alias)] = member
		}
		return ref
	}
	ref.names[lhs] = "*"
	return ref
}

func jsSignature(raw []string, i int) string {
	s := strings.TrimSpace(raw[i])
	if j := strings.Index(s, "{"); j > 0 && !strings.HasPrefix(s, "{") {
		s = strings.TrimSpace(s[:j])
	}
	if len(s) > 160 {
		s = s[:159] + "…"
	}
	return s
}

// jsDoc returns the JSDoc/line comment directly above line i.
func jsDoc(raw []string, i int) string {
	var parts []string
	for j := i - 1; j >= 0 && j >= i-15; j-- {
		t := strings.TrimSpace(raw[j])
		switch {
		case strings.HasPrefix(t, "//"):
			parts = append([]string{strings.TrimSpace(strings.TrimPrefix(t, "//"))}, parts...)
		case strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/**") || strings.HasSuffix(t, "*/"):
			t = strings.TrimSpace(strings.Trim(strings.TrimPrefix(strings.TrimPrefix(t, "/**"), "*"), "/ "))
			if strings.HasPrefix(t, "@") {
				continue
			}
			if t != "" {
				parts = append([]string{t}, parts...)
			}
			if strings.HasPrefix(strings.TrimSpace(raw[j]), "/**") {
				return strings.Join(parts, " ")
			}
		case strings.HasPrefix(t, "@") || t == "":
			if t == "" && len(parts) > 0 {
				return strings.Join(parts, " ")
			}
			if t == "" {
				return ""
			}
		default:
			return strings.Join(parts, " ")
		}
	}
	return strings.Join(parts, " ")
}

var vueScriptRe = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)

// vueScript returns the <script> block of a Vue SFC and its line offset.
func vueScript(src []byte) ([]byte, int) {
	m := vueScriptRe.FindSubmatchIndex(src)
	if m == nil {
		return nil, 0
	}
	start := m[4]
	return src[start:m[5]], strings.Count(string(src[:start]), "\n")
}
