package lang

import (
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// A structural parser for languages whose blocks are braces (Java, Kotlin,
// Rust, C#;
// docs/PLAN.md, agent workflow roadmap step 14). Each language is a
// braceLang: how a type and a function are declared, which words are not
// calls, how imports and annotations look, and its rules and routes. The
// engine masks strings and comments, nests types by brace depth, attributes
// calls to the innermost declaration and leaves linking to link().

type braceLang struct {
	lang string
	// typeDecl matches a type declaration at the start of a line; the last
	// non-empty group is the name.
	typeDecl *regexp.Regexp
	// funcDecl matches a function or method declaration; group "name" is
	// its name. member reports whether a line inside a type body really
	// declares one (not a field initialiser or a statement).
	funcDecl *regexp.Regexp
	member   func(line, name string) bool
	// topFuncs allows functions outside any type (Kotlin, not Java).
	topFuncs bool
	// annotation matches a line that only annotates the next declaration.
	annotation *regexp.Regexp
	// pkgDecl declares the file's package or namespace (group 1). With
	// blockPkg, a declaration that opens a block (C#'s namespace X { … })
	// does not nest: what sits at its depth counts as top level.
	pkgDecl  *regexp.Regexp
	blockPkg bool
	imports  func(raw string, line int) (importRef, bool)
	keywords map[string]bool
	branches []string
	exported func(decl string) bool
	// testAnnotated reports a test function from its annotations.
	testAnnotated func(annotations []string) bool
	textBlocks    bool // """ … """ strings (Java text blocks, Kotlin raw strings, C# raw strings)
	verbatim      bool // C#'s @"…" strings, which span lines and escape " as ""
	// rust: 'a is a lifetime unless it closes as a character literal, and
	// r"…" / r#"…"# are raw strings.
	rust bool
	// scope matches a block that gives its methods a type without declaring
	// one (Rust's impl Type, impl Trait for Type); its last non-empty group
	// is the type.
	scope *regexp.Regexp
	rules func(raw, masked string, line int, owner string) []finding
	// routes reads the entry points of a function from its annotations and
	// those of the enclosing type.
	routes func(typeAnns, fnAnns []string, handler string, line int) []route
	// lineRoutes finds routes registered by a statement (Ktor's get("/x") { }).
	lineRoutes func(raw string, line int) []route
}

// braceCall finds calls: plain f(…), and recv.f(…), recv::f(…), recv->f(…).
var braceCall = regexp.MustCompile(`(?:([A-Za-z_$][\w$]*)\s*(?:\?\.|\.|::|->)\s*)?([A-Za-z_$][\w$]*)\s*(?:<[^<>()]*>)?\s*\(`)

type braceType struct {
	name  string
	depth int // brace depth of the type's body
	end   int
	anns  []string
}

// parseBrace parses one file of language l.
func parseBrace(src []byte, l *braceLang) (*fileResult, string) {
	raw := strings.Split(string(src), "\n")
	masked := maskBraceLang(string(src), l)
	lines := strings.Split(masked, "\n")
	depth := depths(masked)
	fr := &fileResult{partial: unbalanced(masked, '{', '}')}
	pkg := ""

	var types, spaces []braceType // spaces: open namespace blocks
	var pending []string          // annotation lines above the next declaration
	for i, line := range lines {
		n := i + 1
		for len(types) > 0 && n > types[len(types)-1].end {
			types = types[:len(types)-1]
		}
		for len(spaces) > 0 && n > spaces[len(spaces)-1].end {
			spaces = spaces[:len(spaces)-1]
		}
		top := 0
		if len(spaces) > 0 {
			top = spaces[len(spaces)-1].depth
		}
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if l.pkgDecl != nil && depth[i] == top && len(types) == 0 {
			if m := l.pkgDecl.FindStringSubmatch(raw[i]); m != nil {
				if pkg == "" {
					pkg = m[1]
				}
				if l.blockPkg && !strings.HasSuffix(t, ";") {
					spaces = append(spaces, braceType{name: m[1], depth: depth[i] + 1, end: braceEnd(lines, n)})
				}
				continue
			}
		}
		if depth[i] == top && l.imports != nil {
			if imp, ok := l.imports(raw[i], n); ok {
				fr.imports = append(fr.imports, imp)
				continue
			}
		}
		if l.annotation != nil && l.annotation.MatchString(line) {
			pending = append(pending, annotationText(raw, i))
			if !strings.Contains(line, "{") && !l.typeDecl.MatchString(line) && !l.funcDecl.MatchString(line) {
				continue
			}
		}
		atMember := len(types) > 0 && depth[i] == types[len(types)-1].depth
		atTop := depth[i] == top && len(types) == 0
		if l.scope != nil && (atTop || atMember) {
			if m := l.scope.FindStringSubmatch(line); m != nil {
				types = append(types, braceType{name: lastGroup(m), depth: depth[i] + 1, end: braceEnd(lines, n), anns: pending})
				pending = nil
				continue
			}
		}
		if (atTop || atMember) && l.typeDecl.MatchString(line) {
			name := lastGroup(l.typeDecl.FindStringSubmatch(line))
			if name == "" {
				name = "Companion" // Kotlin's unnamed companion object
			}
			if len(types) > 0 {
				name = types[len(types)-1].name + "." + name
			}
			end := braceEnd(lines, n)
			fr.symbols = append(fr.symbols, &symbol{name: name, kind: model.KindType, start: n, end: end,
				signature: braceSignature(raw, i), exported: l.exported(line), doc: braceDoc(raw, i-len(pending))})
			types = append(types, braceType{name: name, depth: depth[i] + 1, end: end, anns: pending})
			pending = nil
			continue
		}
		if m := l.funcDecl.FindStringSubmatch(line); m != nil && (atMember || atTop && l.topFuncs) {
			name := m[l.funcDecl.SubexpIndex("name")]
			if l.keywords[name] || l.member != nil && !l.member(line, name) {
				pending = nil
				continue
			}
			s := &symbol{name: name, kind: model.KindFunction, start: n, end: braceEnd(lines, n), signature: braceSignature(raw, i),
				exported: l.exported(line), doc: braceDoc(raw, i-len(pending))}
			var typeAnns []string
			if atMember {
				cls := types[len(types)-1]
				s.class, s.name, s.kind = cls.name, cls.name+"."+name, model.KindMethod
				typeAnns = cls.anns
			}
			if l.testAnnotated != nil && l.testAnnotated(pending) {
				s.isTest = true
			}
			if len(pending) > 0 {
				s.root = true // annotated: a framework calls it
			}
			if l.routes != nil {
				fr.routes = append(fr.routes, l.routes(typeAnns, pending, s.name, n)...)
			}
			fr.symbols = append(fr.symbols, s)
			pending = nil
			continue
		}
		pending = nil
	}

	// Calls and findings go to the innermost declaration around the line.
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
	for i, line := range lines {
		n := i + 1
		o := owners[n]
		for _, m := range braceCall.FindAllStringSubmatch(line, -1) {
			if l.keywords[m[2]] || o != nil && o.start == n && lastPart(o.name) == m[2] {
				continue
			}
			c := call{recv: m[1], name: m[2], line: n}
			if o != nil {
				o.calls = append(o.calls, c)
			} else {
				fr.topCalls = append(fr.topCalls, c)
			}
		}
		if l.rules != nil {
			fr.findings = append(fr.findings, l.rules(raw[i], line, n, ownerName(o))...)
		}
		if l.lineRoutes != nil && strings.TrimSpace(line) != "" { // not in a comment
			fr.routes = append(fr.routes, l.lineRoutes(raw[i], n)...)
		}
	}
	for _, s := range fr.symbols {
		if s.kind == model.KindType {
			continue
		}
		body := strings.Join(lines[max(0, s.start-1):min(len(lines), s.end)], "\n")
		s.complexity = countBranches(body, l.branches) + strings.Count(body, "&&") + strings.Count(body, "||")
	}
	return fr, pkg
}

// annotationText is an annotation with its arguments, joined when they
// span lines (@RequestMapping(\n value = "/x")).
func annotationText(raw []string, i int) string {
	text := strings.TrimSpace(raw[i])
	for j := i + 1; j < len(raw) && j < i+8 && strings.Count(text, "(") > strings.Count(text, ")"); j++ {
		text += " " + strings.TrimSpace(raw[j])
	}
	return text
}

// braceEnd is where the declaration starting at line start ends: its
// closing brace, or the line itself for declarations without a body
// (abstract methods, Kotlin's fun f() = expr).
func braceEnd(lines []string, start int) int {
	for i := start - 1; i < len(lines) && i < start+3; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.Contains(t, "{") {
			return blockEnd(lines, start)
		}
		if strings.HasSuffix(t, ";") || strings.Contains(t, ")=") || strings.Contains(t, ") =") || strings.Contains(t, "=>") {
			return i + 1
		}
	}
	return blockEnd(lines, start)
}

func lastGroup(m []string) string {
	for i := len(m) - 1; i > 0; i-- {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

func braceSignature(raw []string, i int) string {
	return jsSignature(raw, i)
}

// braceDoc reads the comment block right above line i: /** */, ///, //!,
// // and * lines, without markers and @tags.
func braceDoc(raw []string, i int) string {
	var parts []string
	for j := i - 1; j >= 0 && j >= i-20; j-- {
		t := strings.TrimSpace(raw[j])
		if !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "*") && !strings.HasPrefix(t, "/*") {
			break
		}
		t = strings.TrimSpace(strings.TrimLeft(strings.TrimSuffix(t, "*/"), "/*!"))
		t = strings.TrimSpace(docTag.ReplaceAllString(t, ""))
		if t == "" || strings.HasPrefix(t, "@") {
			continue
		}
		parts = append([]string{t}, parts...)
	}
	return strings.Join(parts, " ")
}

// docTag is the markup of C# XML doc comments (<summary>) and Javadoc
// (<p>), not generics in prose.
var docTag = regexp.MustCompile(`</?(?:summary|remarks|returns|value|para|p|c|code|example|exception|param|typeparam|see|seealso|inheritdoc)\b[^>]*>`)

func maskBraceLang(src string, l *braceLang) string {
	if l.rust {
		return maskRust(src)
	}
	return maskBrace(src, l.textBlocks, l.verbatim)
}

// maskBrace blanks string and character literal contents and comments,
// keeping line structure, for C-like languages.
func maskBrace(src string, textBlocks, verbatim bool) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
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
		case textBlocks && strings.HasPrefix(src[i:], `"""`):
			j := i + 3
			for j < len(b) && !strings.HasPrefix(src[j:], `"""`) {
				blank(j)
				j++
			}
			i = min(j+2, len(b)-1)
		case verbatim && (strings.HasPrefix(src[i:], `@"`) || strings.HasPrefix(src[i:], `$@"`) || strings.HasPrefix(src[i:], `@$"`)):
			j := i + strings.IndexByte(src[i:], '"') + 1
			for j < len(b) && (b[j] != '"' || j+1 < len(b) && b[j+1] == '"') {
				if b[j] == '"' { // "" is a quote inside the string
					blank(j)
					j++
				}
				blank(j)
				j++
			}
			i = j
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
		}
	}
	return string(out)
}

// maskRust is maskBrace for Rust: raw strings r"…" and r#"…"#, and 'a
// lifetimes, which are not character literals.
func maskRust(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
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
		case c == 'r' && i+1 < len(b) && (b[i+1] == '"' || b[i+1] == '#') && (i == 0 || !isIdent(rune(b[i-1]))):
			j := i + 1
			hashes := 0
			for j < len(b) && b[j] == '#' {
				hashes++
				j++
			}
			if j >= len(b) || b[j] != '"' {
				continue
			}
			end := "\"" + strings.Repeat("#", hashes)
			k := strings.Index(src[j+1:], end)
			if k < 0 {
				k = len(src) - j - 1
			}
			for m := j + 1; m < j+1+k; m++ {
				blank(m)
			}
			i = j + k + len(end)
		case c == '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
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
		case c == '\'':
			// A character literal closes within a few bytes ('a', '\n',
			// '\u{1F600}'); otherwise this is a lifetime ('a, 'static).
			if j := strings.IndexByte(src[i+1:min(len(src), i+12)], '\''); j >= 0 && (j == 1 || b[i+1] == '\\' || j <= 4 && b[i+1] >= 0x80) {
				for m := i + 1; m < i+1+j; m++ {
					blank(m)
				}
				i += j + 1
			}
		}
	}
	return string(out)
}
