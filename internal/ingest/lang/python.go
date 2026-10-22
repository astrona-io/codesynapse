package lang

import (
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

var (
	pyDef        = regexp.MustCompile(`^(\s*)(async\s+)?def\s+([A-Za-z_]\w*)\s*\(([^)]*)\)?`)
	pyClass      = regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)\s*(\([^)]*\))?\s*:`)
	pyImport     = regexp.MustCompile(`^\s*import\s+(.+)$`)
	pyFromImport = regexp.MustCompile(`^\s*from\s+(\.*[\w.]*)\s+import\s+(.+)$`)
	pyCall       = regexp.MustCompile(`(?:\b([A-Za-z_]\w*)\s*\.\s*)?\b([A-Za-z_]\w*)\s*\(`)
	pyDecorator  = regexp.MustCompile(`^\s*@([\w.]+)`)
	pyKeywords   = map[string]bool{"if": true, "for": true, "while": true, "return": true, "print": true, "elif": true, "not": true, "and": true, "or": true, "in": true, "def": true, "class": true, "with": true, "assert": true, "lambda": true, "yield": true, "await": true, "super": true}
	pyBranches   = []string{"if", "elif", "for", "while", "except", "and", "or", "case"}
)

type pyLine struct {
	text   string
	indent int
	blank  bool // blank, comment-only, or inside a multi-line string
}

// pyLines classifies lines, masking string contents so "def" inside a
// docstring is not a declaration and quotes do not confuse call matching.
func pyLines(src []byte) []pyLine {
	raw := strings.Split(string(src), "\n")
	out := make([]pyLine, len(raw))
	inTriple := ""
	for i, l := range raw {
		l = strings.TrimRight(l, "\r")
		masked, still := maskPyStrings(l, inTriple)
		startedInString := inTriple != ""
		inTriple = still
		trim := strings.TrimSpace(masked)
		out[i] = pyLine{text: masked, indent: len(l) - len(strings.TrimLeft(l, " \t")), blank: trim == "" || strings.HasPrefix(trim, "#") || startedInString && inTriple != ""}
	}
	return out
}

// maskPyStrings replaces string literal contents with spaces and strips
// comments. open is the triple quote still open from previous lines.
func maskPyStrings(l, open string) (string, string) {
	var b strings.Builder
	i := 0
	for i < len(l) {
		if open != "" {
			j := strings.Index(l[i:], open)
			if j < 0 {
				b.WriteString(strings.Repeat(" ", len(l)-i))
				return b.String(), open
			}
			b.WriteString(strings.Repeat(" ", j) + open)
			i += j + len(open)
			open = ""
			continue
		}
		c := l[i]
		switch {
		case c == '#':
			return b.String(), ""
		case strings.HasPrefix(l[i:], `"""`) || strings.HasPrefix(l[i:], `'''`):
			open = l[i : i+3]
			b.WriteString(open)
			i += 3
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(l) && l[j] != c {
				if l[j] == '\\' {
					j++
				}
				j++
			}
			b.WriteByte(c)
			b.WriteString(strings.Repeat(" ", max(0, min(j, len(l))-i-1)))
			if j < len(l) {
				b.WriteByte(c)
			}
			i = j + 1
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), open
}

func parsePython(src []byte) *fileResult {
	lines := pyLines(src)
	raw := strings.Split(string(src), "\n")
	fr := &fileResult{}

	var stack []openSym
	var pendingDecorators []string
	closeTo := func(indent, line int) {
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack[len(stack)-1].sym.end = line
			stack = stack[:len(stack)-1]
		}
	}
	lastCode := 0
	for i, l := range lines {
		n := i + 1
		if l.blank {
			continue
		}
		closeTo(l.indent, lastCode)
		lastCode = n
		if m := pyDecorator.FindStringSubmatch(l.text); m != nil {
			pendingDecorators = append(pendingDecorators, m[1])
			continue
		}
		if m := pyClass.FindStringSubmatch(l.text); m != nil {
			s := &symbol{name: m[2], kind: model.KindType, start: n - len(pendingDecorators), signature: "class " + m[2] + m[3], exported: !strings.HasPrefix(m[2], "_")}
			if len(stack) > 0 {
				s.name = stack[len(stack)-1].sym.name + "." + m[2]
			}
			s.isTest = strings.HasPrefix(m[2], "Test")
			s.doc = pyDocstring(raw, i+1)
			fr.symbols = append(fr.symbols, s)
			stack = append(stack, openSym{s, l.indent})
			pendingDecorators = nil
			continue
		}
		if m := pyDef.FindStringSubmatch(l.text); m != nil {
			name := m[3]
			s := &symbol{name: name, kind: model.KindFunction, start: n - len(pendingDecorators), signature: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw[i]), ":")), exported: !strings.HasPrefix(name, "_")}
			// Enclosing class makes it a method; nested functions belong to their parent.
			for j := len(stack) - 1; j >= 0; j-- {
				if stack[j].sym.kind == model.KindType {
					s.class = stack[j].sym.name
					s.name = s.class + "." + name
					s.kind = model.KindMethod
					break
				}
				if stack[j].sym.kind != model.KindType {
					s = nil
					break
				}
			}
			if s == nil { // nested def: its body is part of the parent
				pendingDecorators = nil
				continue
			}
			s.isTest = strings.HasPrefix(name, "test_") || name == "test"
			s.root = len(pendingDecorators) > 0 && !onlyBuiltinDecorators(pendingDecorators)
			s.doc = pyDocstring(raw, i+1)
			fr.symbols = append(fr.symbols, s)
			stack = append(stack, openSym{s, l.indent})
			pendingDecorators = nil
			continue
		}
		pendingDecorators = nil
		if strings.Contains(l.text, `__name__`) && strings.Contains(l.text, `__main__`) {
			continue
		}
		if m := pyFromImport.FindStringSubmatch(l.text); m != nil && l.indent == 0 {
			fr.imports = append(fr.imports, pyFrom(m[1], m[2], n))
			continue
		}
		if m := pyImport.FindStringSubmatch(l.text); m != nil && l.indent == 0 {
			for _, part := range strings.Split(m[1], ",") {
				mod, alias, _ := strings.Cut(strings.TrimSpace(part), " as ")
				mod = strings.TrimSpace(mod)
				local := strings.TrimSpace(alias)
				if local == "" {
					local, _, _ = strings.Cut(mod, ".")
				}
				fr.imports = append(fr.imports, importRef{spec: mod, names: map[string]string{local: "*"}, line: n})
			}
			continue
		}
		calls := pyCalls(l.text, n)
		owner := innermostFunc(stack)
		if owner != nil {
			owner.calls = append(owner.calls, calls...)
		} else {
			fr.topCalls = append(fr.topCalls, calls...)
		}
		fr.findings = append(fr.findings, pyRules(raw[i], l.text, n, ownerName(owner))...)
	}
	closeTo(-1, lastCode)
	for _, s := range fr.symbols {
		if s.kind == model.KindType {
			continue
		}
		body := strings.Join(maskedRange(lines, s.start, s.end), "\n")
		s.complexity = countBranches(body, pyBranches)
	}
	return fr
}

func onlyBuiltinDecorators(ds []string) bool {
	for _, d := range ds {
		switch d {
		case "staticmethod", "classmethod", "property", "abstractmethod", "abc.abstractmethod", "functools.wraps", "wraps", "cache", "lru_cache", "functools.lru_cache", "functools.cache", "override", "typing.override":
		default:
			return false
		}
	}
	return true
}

// openSym is a declaration whose body is still being read.
type openSym struct {
	sym    *symbol
	indent int
}

func innermostFunc(stack []openSym) *symbol {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].sym.kind != model.KindType {
			return stack[i].sym
		}
	}
	return nil
}

func ownerName(s *symbol) string {
	if s == nil {
		return ""
	}
	return s.name
}

func maskedRange(lines []pyLine, start, end int) []string {
	var out []string
	for i := max(start, 1); i <= end && i <= len(lines); i++ {
		out = append(out, lines[i-1].text)
	}
	return out
}

func pyFrom(mod, names string, line int) importRef {
	ref := importRef{spec: mod, names: map[string]string{}, line: line}
	names = strings.Trim(strings.TrimSpace(names), "()")
	for _, part := range strings.Split(names, ",") {
		member, alias, _ := strings.Cut(strings.TrimSpace(part), " as ")
		member, alias = strings.TrimSpace(member), strings.TrimSpace(alias)
		if member == "" || member == "*" {
			continue
		}
		if alias == "" {
			alias = member
		}
		ref.names[alias] = member
	}
	return ref
}

func pyCalls(masked string, line int) []call {
	var out []call
	for _, m := range pyCall.FindAllStringSubmatch(masked, -1) {
		if pyKeywords[m[2]] {
			continue
		}
		out = append(out, call{recv: m[1], name: m[2], line: line})
	}
	return out
}

// pyDocstring returns the docstring starting on the line after a def/class.
func pyDocstring(raw []string, next int) string {
	for i := next; i < len(raw) && i < next+3; i++ {
		t := strings.TrimSpace(raw[i])
		if t == "" {
			continue
		}
		for _, q := range []string{`"""`, `'''`} {
			if strings.HasPrefix(t, q) {
				body := strings.TrimPrefix(t, q)
				if j := strings.Index(body, q); j >= 0 {
					return body[:j]
				}
				var b strings.Builder
				b.WriteString(body)
				for k := i + 1; k < len(raw) && k < i+20; k++ {
					if j := strings.Index(raw[k], q); j >= 0 {
						b.WriteString(" " + strings.TrimSpace(raw[k][:j]))
						break
					}
					b.WriteString(" " + strings.TrimSpace(raw[k]))
				}
				return b.String()
			}
		}
		return ""
	}
	return ""
}
