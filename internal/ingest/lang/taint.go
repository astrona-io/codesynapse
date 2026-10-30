package lang

import (
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Taint tracking inside one function for Python and JavaScript/TypeScript
// (docs/PLAN.md, security roadmap item 3, fifth step). Request input of the
// common web frameworks is followed through local assignments, string
// concatenation, f-strings, template literals, .format and % to SQL text,
// process execution, file paths and outbound URLs, the sinks the Go rules
// sql-from-request, exec-from-request, path-from-request and
// ssrf-from-request check.
//
// The parsers are structural, so this is an approximation that prefers
// missing a flow to reporting a guess:
//   - Statements are lines, joined while brackets are open (at most
//     maxJoin lines); a value is tracked by its local name only, not
//     through attributes or containers.
//   - Inside one file, a call carries the taint of its arguments to its
//     result. At ingest, calls to the repository's own functions apply
//     their summaries instead (taintflow.go): the result carries only what
//     the callee returns, and request input passed in is reported at the
//     callee's sinks.
//   - A JavaScript name is forgotten when the block it was assigned in
//     closes; a Python name lives for the whole function.
//   - Reassigning a name with a clean value clears it; a value that went
//     into a reported sink is not reported again.
//   - A value passed through a number conversion, a base name, shell or URL
//     quoting is clean, and so is a comparison. URL quoting is treated as
//     clean everywhere, like url.QueryEscape in the Go detector.
//   - Only the argument that holds the SQL text, command, path or URL is
//     checked: a parameterised query with request values in its separate
//     arguments is not a finding, nor is a URL with a fixed scheme and host.

// maxJoin caps how many lines one statement may span.
const maxJoin = 10

// taintLang is what differs between the languages.
type taintLang struct {
	sources    *regexp.Regexp
	guards     []string        // a literal every source match contains
	sanitizers map[string]bool // call names whose result is clean
	sinks      []taintSink
	// objectKeys: an identifier followed by ':' is a key, not a value
	// (JavaScript object literals and TypeScript annotations).
	objectKeys bool
	// skipSource rejects a source match by what follows it, such as
	// Flask's request.args.get("n", type=int).
	skipSource func(expr string, end int) bool
}

// taintSink is a call whose argument must not carry request input.
type taintSink struct {
	rule string
	head *regexp.Regexp // the callee, ending at the opening parenthesis
	arg  int            // the argument checked; -1 means every argument
	kw   string         // Python keyword that names the same argument
	what string         // what the argument is, for the finding detail
	// childProcess sinks only count in files importing child_process.
	childProcess bool
}

// taint is what a value carries: the request input it comes from and,
// while a function is summarised, which of its parameters (bit i is
// parameter i).
type taint struct {
	from   string
	params uint64
}

func (t taint) none() bool { return t.from == "" && t.params == 0 }

// or merges u into t; t's request input wins.
func (t taint) or(u taint) taint {
	if t.from == "" {
		t.from = u.from
	}
	t.params |= u.params
	return t
}

// taintVar is a tainted local and the depth of the block it was set in.
type taintVar struct {
	t     taint
	depth int
}

type taintState struct {
	lang *taintLang
	vars map[string]taintVar

	// The rest is for calls between functions (taintflow.go); all zero
	// when one file is scanned on its own.
	// resolve finds the repository function a call names, or nil.
	resolve func(class, recv, name string) *taintFn
	class   string // the class of the code being scanned, for self.f()
	// decl is the name a statement declares (function f(a) {): not a call.
	decl string
	// fn is the function being summarised; its findings are not reported.
	fn *taintFn
	// entry names the function request input is read in, and cross takes
	// the findings that input causes in the functions it is passed to.
	entry string
	cross func(ref taintRef, path []string, from, entry string)
}

// stmt is one statement: its first line, the masked code and the raw text
// at the same offsets.
type stmt struct {
	line      int
	code, raw string
}

var (
	callHead = regexp.MustCompile(`[A-Za-z_$][\w$]*(?:\s*\.\s*[A-Za-z_$][\w$]*)*\s*\(`)
	identRe  = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
	fixedURL = regexp.MustCompile("^(?:[rRbBfF]{0,2}[\"'`])https?://[^/\"'`{}$\\s]+/")
)

// relevant is a cheap test that a statement can matter: it mentions a
// tainted name or a request source, or calls a function that returns
// request input. A statement that does none of these can neither reach a
// sink nor change what a name carries.
func (st *taintState) relevant(code string) bool {
	for _, l := range st.lang.guards {
		if strings.Contains(code, l) {
			if st.lang.sources.MatchString(code) {
				return true
			}
			break
		}
	}
	if len(st.vars) > 0 {
		for _, m := range identRe.FindAllStringIndex(code, -1) {
			if _, ok := st.vars[code[m[0]:m[1]]]; ok {
				return true
			}
		}
	}
	if st.resolve != nil {
		for _, m := range callHead.FindAllStringIndex(code, -1) {
			if fn := st.callee(code, m); fn != nil && fn.sum.from != "" {
				return true
			}
		}
	}
	return false
}

func hasInputs(funcs []*symbol) bool {
	for _, s := range funcs {
		if len(s.inputs) > 0 {
			return true
		}
	}
	return false
}

// taintOf returns what expr carries: the request input it is built from
// and, while summarising, the parameters.
func (st *taintState) taintOf(expr string) taint { return st.taintAt(expr, 0) }

// maxCallNest bounds how deeply calls nested in arguments are followed.
const maxCallNest = 4

func (st *taintState) taintAt(expr string, nest int) taint {
	expr, called := st.blankCalls(expr, nest)
	if strings.Contains(expr, "==") || strings.Contains(expr, "!=") {
		if !strings.Contains(expr, "?") && !strings.Contains(expr, " if ") {
			return taint{} // a comparison is a boolean
		}
	}
	var t taint
	for _, m := range st.lang.sources.FindAllStringIndex(expr, -1) {
		if m[0] > 0 && (isIdentByte(expr[m[0]-1]) || expr[m[0]-1] == '.' && !strings.HasPrefix(expr[m[0]:], "request.")) {
			continue
		}
		if st.lang.skipSource != nil && st.lang.skipSource(expr, m[1]) {
			continue
		}
		t.from = strings.Join(strings.Fields(expr[m[0]:m[1]]), "")
		break
	}
	if len(st.vars) == 0 {
		return t.or(called)
	}
	for _, m := range identRe.FindAllStringIndex(expr, -1) {
		if m[0] > 0 && (expr[m[0]-1] == '.' || isIdentByte(expr[m[0]-1])) {
			continue
		}
		v, ok := st.vars[expr[m[0]:m[1]]]
		if !ok {
			continue
		}
		rest := strings.TrimLeft(expr[m[1]:], " \t")
		if strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==") {
			continue // a keyword argument's name
		}
		if st.lang.objectKeys && strings.HasPrefix(rest, ":") {
			continue
		}
		t = t.or(v.t)
	}
	return t.or(called)
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// blankCalls replaces calls to sanitizers and to summarised repository
// functions, arguments included, with spaces: int(request.args["n"])
// carries nothing, and a summarised call carries only what its callee
// returns, which is the second result.
func (st *taintState) blankCalls(expr string, nest int) (string, taint) {
	var b []byte
	var t taint
	skip := 0 // calls inside a blanked call are its arguments
	for _, m := range callHead.FindAllStringIndex(expr, -1) {
		if m[0] < skip || m[0] > 0 && expr[m[0]-1] == '.' {
			continue
		}
		var fn *taintFn
		if !st.lang.sanitizers[callName(expr[m[0]:m[1]])] {
			if nest >= maxCallNest {
				continue
			}
			if fn = st.callee(expr, m); fn == nil {
				continue
			}
		}
		if b == nil {
			b = []byte(expr)
		}
		end := closingParen(expr, m[1])
		if fn != nil {
			t = t.or(st.returned(fn, expr[m[1]:], nest+1))
		}
		for i := m[0]; i < end; i++ {
			b[i] = ' '
		}
		skip = end
	}
	if b == nil {
		return expr, t
	}
	return string(b), t
}

// callName is a call head without spaces or the parenthesis: "os.path.basename".
func callName(head string) string {
	return strings.Join(strings.Fields(strings.TrimSuffix(head, "(")), "")
}

// closingParen returns the offset after the parenthesis closing the one
// opened just before from, or len(s) when it is not closed.
func closingParen(s string, from int) int {
	depth := 0
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return i + 1
			}
			depth--
		}
	}
	return len(s)
}

// sinks reports the sinks of one statement that receive request input,
// adds those a parameter reaches to the summary being built, and says
// whether any sink received something.
func (st *taintState) sinks(s stmt, owner string, childProcess bool) ([]finding, bool) {
	var out []finding
	hit := false
	done := map[string]bool{}
	for _, sk := range st.lang.sinks {
		if done[sk.rule] || sk.childProcess && !childProcess {
			continue
		}
		for _, m := range sk.head.FindAllStringIndex(s.code, -1) {
			t := st.sinkArg(sk, s, m[1])
			if t.none() {
				continue
			}
			hit = true
			base := calleeAt(s.code, m[1]) + ": " + sk.what + " comes from the incoming request"
			if st.fn != nil {
				for i := range maxParams {
					if t.params&(1<<i) != 0 {
						st.fn.sum.addSink(i, taintRef{rule: sk.rule, rel: st.fn.src.rel, line: s.line, owner: owner, base: base})
					}
				}
				continue // a summary reports nothing itself
			}
			if t.from == "" {
				continue
			}
			out = append(out, finding{rule: sk.rule, detail: base + " (via " + t.from + ")", line: s.line, owner: owner})
			done[sk.rule] = true
			break
		}
	}
	return out, hit
}

// calleeAt returns the dotted name of the call whose "(" ends at end - 1:
// "cursor.execute", "fs.readFile".
func calleeAt(code string, end int) string {
	i := end - 1
	for i > 0 && (code[i-1] == ' ' || code[i-1] == '\t') {
		i--
	}
	j := i
	for j > 0 && (isIdentByte(code[j-1]) || code[j-1] == '.') {
		j--
	}
	return code[j:i]
}

// sinkArg returns what reaches the checked argument of the call whose
// arguments start at off.
func (st *taintState) sinkArg(sk taintSink, s stmt, off int) taint {
	spans := argSpans(s.code[off:])
	var picked [][2]int
	for i, sp := range spans {
		a := s.code[off+sp[0] : off+sp[1]]
		if sk.kw != "" {
			if name, val, ok := strings.Cut(a, "="); ok && strings.TrimSpace(name) == sk.kw && !strings.HasPrefix(val, "=") {
				picked = append(picked, [2]int{sp[1] - len(val), sp[1]})
				continue
			}
		}
		if (sk.arg < 0 || i == sk.arg) && !isKeywordArg(a) {
			picked = append(picked, sp)
		}
	}
	var t taint
	for _, sp := range picked {
		a := strings.TrimSpace(s.code[off+sp[0] : off+sp[1]])
		if a == "" || a[0] == '{' || a[0] == '[' && sk.rule == "sql-from-request" {
			continue // an options object or a parameter list, not the text
		}
		if sk.rule == "ssrf-from-request" && fixedURL.MatchString(strings.TrimSpace(s.raw[off+sp[0]:min(len(s.raw), off+sp[1])])) {
			continue // the client chooses a path on a fixed host at most
		}
		t = t.or(st.taintOf(a))
	}
	return t
}

func isKeywordArg(a string) bool {
	m := identRe.FindStringIndex(a)
	if m == nil || m[0] != 0 {
		return false
	}
	rest := strings.TrimLeft(a[m[1]:], " \t")
	return strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "==")
}

// assign records the names a statement assigns: tainted by a tainted value,
// cleared by a clean one (augmented assignments only add).
func (st *taintState) assign(names []string, rhs string, augmented bool, depth int) {
	t := st.taintOf(rhs)
	for _, n := range names {
		switch {
		case !t.none():
			if v, ok := st.vars[n]; ok && augmented {
				t = v.t.or(t)
			}
			st.vars[n] = taintVar{t: t, depth: depth}
		case !augmented:
			delete(st.vars, n)
		}
	}
}

// supersede drops the pattern findings a taint finding on the same line
// replaces, as the Go detector reports sql-from-request instead of
// sql-concat, and keeps one finding per rule and line.
func supersede(pattern, taint []finding) []finding {
	if len(taint) == 0 {
		return pattern
	}
	replaced := map[string][]string{
		"sql-from-request":  {"sql-concat"},
		"exec-from-request": {"exec-shell", "exec-variable"},
		"path-from-request": {"path-from-request"},
	}
	type key struct {
		line int
		rule string
	}
	drop := map[key]bool{}
	for _, f := range taint {
		for _, r := range replaced[f.rule] {
			drop[key{f.line, r}] = true
		}
	}
	out := pattern[:0:0]
	for _, f := range pattern {
		if !drop[key{f.line, f.rule}] {
			out = append(out, f)
		}
	}
	seen := map[key]bool{}
	for _, f := range taint {
		if k := (key{f.line, f.rule}); !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

// joinStatements groups lines [from, to] (1-based) into statements. open
// says whether a line leaves the statement open; skip drops lines that
// are no statement (blank, inside a string).
func joinStatements(code, raw []string, from, to int, open func(depth int, line string) bool, skip func(i int) bool) []stmt {
	var out []stmt
	for i := max(from, 1); i <= to && i <= len(code); {
		if skip != nil && skip(i) {
			i++
			continue
		}
		var c, r strings.Builder
		depth, n := 0, i
		for ; n <= to && n <= len(code) && n < i+maxJoin; n++ {
			l := code[n-1]
			rl := raw[n-1]
			if len(rl) < len(l) {
				rl += strings.Repeat(" ", len(l)-len(rl))
			}
			c.WriteString(l + " ")
			r.WriteString(rl[:len(l)] + " ")
			depth += bracketDelta(l)
			if !open(depth, l) {
				n++
				break
			}
		}
		out = append(out, stmt{line: i, code: c.String(), raw: r.String()})
		i = n
	}
	return out
}

// bracketDelta counts ( and [ opened minus closed in masked code.
func bracketDelta(l string) int {
	return strings.Count(l, "(") + strings.Count(l, "[") - strings.Count(l, ")") - strings.Count(l, "]")
}

// ---- Python ----

var (
	pySources = regexp.MustCompile(`\brequest\s*\.\s*(?:args|form|values|json|data|files|headers|cookies|get_json|get_data|GET|POST|body|COOKIES|META|query_params|path_params|path_info)\b`)
	pyAssignT = regexp.MustCompile(`^\s*([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)\s*(?::\s*[^=]+?)?\s*(\+?=)\s*([^=].*)$`)
	pyForT    = regexp.MustCompile(`^\s*(?:async\s+)?for\s+([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)\s+in\s+(.+):\s*$`)
	pyTypeArg = regexp.MustCompile(`^\s*\.\s*(?:get|getlist)\s*\(`)

	pyTaint = &taintLang{
		sources: pySources,
		guards:  []string{"request"},
		sanitizers: setOf("int float bool len abs round UUID uuid.UUID os.path.basename posixpath.basename basename " +
			"shlex.quote pipes.quote quote quote_plus urllib.parse.quote urllib.parse.quote_plus parse.quote " +
			"secure_filename werkzeug.utils.secure_filename"),
		skipSource: pyTypedGet,
		sinks: []taintSink{
			{rule: "sql-from-request", head: regexp.MustCompile(`\.\s*(?:execute|executemany|executescript|raw|mogrify)\s*\(`), arg: 0, kw: "sql", what: "the SQL text"},
			{rule: "sql-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:sqlalchemy\.)?text\s*\(`), arg: 0, what: "the SQL text"},
			{rule: "exec-from-request", head: regexp.MustCompile(`(?:^|[^\w.])subprocess\s*\.\s*(?:run|call|Popen|check_output|check_call|getoutput|getstatusoutput)\s*\(`), arg: 0, kw: "args", what: "the command"},
			{rule: "exec-from-request", head: regexp.MustCompile(`(?:^|[^\w.])os\s*\.\s*(?:system|popen|exec\w*|spawn\w*)\s*\(`), arg: -1, what: "the command"},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:io\.|os\.|codecs\.)?open\s*\(`), arg: 0, kw: "file", what: "the path"},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:flask\.)?send_file\s*\(`), arg: 0, kw: "path_or_file", what: "the path"},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w.])FileResponse\s*\(`), arg: 0, kw: "path", what: "the path"},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w.])os\s*\.\s*(?:remove|unlink|rmdir|listdir|makedirs|mkdir)\s*\(`), arg: 0, kw: "path", what: "the path"},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w.])shutil\s*\.\s*(?:rmtree|copy|copyfile|copy2|move)\s*\(`), arg: -1, what: "the path"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:requests|httpx)\s*\.\s*(?:get|post|put|patch|delete|head|options)\s*\(`), arg: 0, kw: "url", what: "the URL"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:requests|httpx)\s*\.\s*request\s*\(`), arg: 1, kw: "url", what: "the URL"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w.])(?:urllib\.request\.)?urlopen\s*\(`), arg: 0, kw: "url", what: "the URL"},
		},
	}
)

// pyTypedGet reports request.args.get("n", type=int): Flask converts it.
func pyTypedGet(expr string, end int) bool {
	rest := expr[end:]
	if m := pyTypeArg.FindStringIndex(rest); m != nil {
		for _, a := range splitArgs(rest[m[1]:]) {
			if name, val, ok := strings.Cut(a, "="); ok && strings.TrimSpace(name) == "type" {
				switch strings.TrimSpace(val) {
				case "int", "float", "bool":
					return true
				}
			}
		}
	}
	return false
}

// fPrefixed reports whether the quote at l[i] opens an f-string.
func fPrefixed(l string, i int) bool {
	j := i
	for j > 0 && i-j < 2 && strings.IndexByte("rRbBuUfF", l[j-1]) >= 0 {
		j--
	}
	if j > 0 && isIdentByte(l[j-1]) {
		return false
	}
	return strings.ContainsAny(l[j:i], "fF")
}

// formatFields blanks an f-string's text and keeps its replacement fields.
func formatFields(s string) string {
	b := []byte(s)
	depth := 0
	for i := 0; i < len(b); i++ {
		switch {
		case depth == 0 && b[i] == '{' && i+1 < len(b) && b[i+1] == '{':
			b[i], b[i+1] = ' ', ' '
			i++
		case b[i] == '{':
			depth++
			b[i] = ' '
		case b[i] == '}' && depth > 0:
			depth--
			b[i] = ' '
		case depth == 0:
			b[i] = ' '
		}
	}
	return string(b)
}

var (
	pyRouteParam = regexp.MustCompile(`<(?:(\w+):)?(\w+)>|\{(\w+)(?::\w+)?\}`)
	pyNumeric    = regexp.MustCompile(`^(?:int|float|bool|UUID|uuid\.UUID|Decimal|datetime\.\w+|date)$`)
	pyFastAPIIn  = regexp.MustCompile(`^(?:fastapi\.)?(?:Query|Header|Cookie|Form|Path|Body)\s*\(`)
	pyStrAnn     = regexp.MustCompile(`\bstr\b`)
)

// pyInputParams returns the parameters of a route handler the client
// chooses: path parameters of the route (<name>, <path:name>, {name}) and
// FastAPI parameters declared as str or with Query(), Header(), Cookie(),
// Form(), Path() or Body(). Numeric converters and annotations are clean,
// Depends() is not input.
func pyInputParams(decorators []string, lines []pyLine, def int) []string {
	pathParams := map[string]bool{}
	routed := false
	for _, d := range decorators {
		m := pyRouteDec.FindStringSubmatch(d)
		if m == nil {
			continue
		}
		routed = true
		for _, p := range pyRouteParam.FindAllStringSubmatch(m[4], -1) {
			switch {
			case p[2] != "" && (p[1] == "int" || p[1] == "float" || p[1] == "uuid"):
			case p[2] != "":
				pathParams[p[2]] = true
			default:
				pathParams[p[3]] = true
			}
		}
	}
	if !routed {
		return nil
	}
	var out []string
	for _, p := range pyParamList(lines, def) {
		decl, def, _ := strings.Cut(p, "=")
		name, ann, _ := strings.Cut(decl, ":")
		name, ann, def = strings.TrimSpace(name), strings.TrimSpace(ann), strings.TrimSpace(def)
		if name == "" || name == "self" || name == "request" || strings.HasPrefix(name, "*") || pyNumeric.MatchString(ann) {
			continue
		}
		if strings.Contains(def, "Depends(") {
			continue
		}
		if pathParams[name] || pyStrAnn.MatchString(ann) || pyFastAPIIn.MatchString(def) {
			out = append(out, name)
		}
	}
	return out
}

// pyParamList returns the parameter declarations of the def on line def,
// as written ("name: str = Query(None)").
func pyParamList(lines []pyLine, def int) []string {
	var sig strings.Builder
	for i := def; i <= len(lines) && i < def+maxJoin; i++ {
		sig.WriteString(lines[i-1].text + " ")
		if bracketDelta(sig.String()) <= 0 && strings.Contains(sig.String(), ")") {
			break
		}
	}
	s := sig.String()
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return nil
	}
	return splitArgs(s[open+1:])
}

// pyParams returns the parameter names of the def on line def, for binding
// call arguments: "*" stands for *args, **kwargs and the bare * that
// starts keyword-only parameters; the / marker is dropped.
func pyParams(lines []pyLine, def int) []string {
	var out []string
	for _, p := range pyParamList(lines, def) {
		decl, _, _ := strings.Cut(p, "=")
		name, _, _ := strings.Cut(decl, ":")
		switch name = strings.TrimSpace(name); {
		case name == "" || name == "/":
		case strings.HasPrefix(name, "*"):
			out = append(out, "*")
		default:
			out = append(out, name)
		}
	}
	return out
}

// pyTaintView masks Python source for taint tracking: strings blanked
// except f-string fields, comments removed, padded to the raw line length.
func pyTaintView(raw []string) []string {
	view := make([]string, len(raw))
	open := ""
	for i, l := range raw {
		l = strings.TrimRight(l, "\r")
		v, still := maskPy(l, open, true)
		open = still
		if len(v) < len(l) {
			v += strings.Repeat(" ", len(l)-len(v))
		}
		view[i] = v
	}
	return view
}

var pyReturnT = regexp.MustCompile(`(?:^|[^\w.])(?:return|yield)\s+(.*)$`)

// pyFindings follows request input inside each function of a parsed
// Python file. cx is nil when the file is scanned on its own.
func (src *taintSource) pyFindings(cx *taintCtx) []finding {
	var funcs []*symbol
	for _, s := range src.symbols {
		if s.kind != model.KindType && s.end >= s.start {
			funcs = append(funcs, s)
		}
	}
	if len(funcs) == 0 || cx == nil && !src.mentions("request") && !hasInputs(funcs) {
		return nil
	}
	var out []finding
	for _, s := range funcs {
		st := &taintState{lang: pyTaint, vars: map[string]taintVar{}, class: s.class, entry: s.name}
		cx.attach(st)
		for _, p := range s.inputs {
			st.vars[p] = taintVar{t: taint{from: "route parameter " + p}}
		}
		out = append(out, src.pyScan(st, s)...)
	}
	return out
}

// pyScan follows what st tracks through the statements of function s.
func (src *taintSource) pyScan(st *taintState, s *symbol) []finding {
	open := func(depth int, l string) bool {
		return depth > 0 || strings.HasSuffix(strings.TrimSpace(l), "\\")
	}
	skip := func(i int) bool { return src.py[i-1].blank }
	var out []finding
	for _, stm := range joinStatements(src.view(), src.raw, s.start, s.end, open, skip) {
		t := strings.TrimSpace(stm.code)
		if strings.HasPrefix(t, "@") || strings.HasPrefix(t, "def ") || strings.HasPrefix(t, "async def ") || strings.HasPrefix(t, "class ") {
			continue
		}
		if !st.relevant(stm.code) {
			continue
		}
		found, hit := st.sinks(stm, s.name, false)
		out = append(out, found...)
		st.through(stm.code)
		if st.fn != nil {
			if m := pyReturnT.FindStringSubmatch(stm.code); m != nil {
				st.returns(m[1])
			}
		}
		if m := pyForT.FindStringSubmatch(stm.code); m != nil {
			st.assign(splitNames(m[1]), m[2], false, 0)
		} else if m := pyAssignT.FindStringSubmatch(stm.code); m != nil {
			names := splitNames(m[1])
			st.assign(names, m[3], m[2] == "+=", 0)
			if hit {
				for _, n := range names {
					delete(st.vars, n) // reported at the sink already
				}
			}
		}
	}
	return out
}

func splitNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ---- JavaScript / TypeScript ----

var (
	jsSources = regexp.MustCompile(`\b(?:req|request|ctx(?:\s*\.\s*request)?)\s*\.\s*(?:query|params|body|headers|cookies|signedCookies)\b|\b(?:req|request)\s*\.\s*(?:get|header|param)\s*\(|\brequest\s*\.\s*(?:json|formData|text)\s*\(|\bnextUrl\s*\.\s*searchParams\b`)
	jsAssignT = regexp.MustCompile(`^\s*(?:(?:export\s+)?(?:const|let|var)\s+)?([A-Za-z_$][\w$]*|\{[^}]*\}|\[[^\]]*\])\s*(?::\s*[^=]+?)?\s*(\+?=)\s*([^=>].*)$`)

	jsTaint = &taintLang{
		sources: jsSources,
		guards:  []string{"req", "ctx", "nextUrl"},
		sanitizers: setOf("Number parseInt parseFloat Boolean Number.parseInt Number.parseFloat Math.floor Math.round " +
			"Math.trunc Math.abs path.basename basename encodeURIComponent escape mysql.escape connection.escape " +
			"SqlString.escape pool.escape db.escape shellescape quote shellQuote.quote"),
		objectKeys: true,
		sinks: []taintSink{
			{rule: "sql-from-request", head: regexp.MustCompile(`\.\s*(?:query|execute|raw|whereRaw|orderByRaw|havingRaw|joinRaw|unsafe|\$queryRawUnsafe|\$executeRawUnsafe)\s*\(`), arg: 0, what: "the SQL text"},
			{rule: "exec-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])(?:(?:child_process|childProcess|cp)\s*\.\s*)?(?:exec|execSync)\s*\(`), arg: 0, what: "the command", childProcess: true},
			{rule: "exec-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])(?:(?:child_process|childProcess|cp)\s*\.\s*)?(?:spawn|spawnSync|execFile|execFileSync|fork)\s*\(`), arg: -1, what: "the command or its arguments", childProcess: true},
			{rule: "path-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])(?:fs|fsp|fse|promises|fs\s*\.\s*promises)\s*\.\s*(?:readFile|readFileSync|writeFile|writeFileSync|appendFile|appendFileSync|createReadStream|createWriteStream|unlink|unlinkSync|rm|rmSync|readdir|readdirSync|open|openSync|copyFile|copyFileSync|mkdir|mkdirSync)\s*\(`), arg: 0, what: "the path"},
			{rule: "path-from-request", head: regexp.MustCompile(`\.\s*(?:sendFile|download)\s*\(`), arg: 0, what: "the path"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])fetch\s*\(`), arg: 0, what: "the URL"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])(?:axios|got|needle|superagent)(?:\s*\.\s*(?:get|post|put|patch|delete|head|options|request))?\s*\(`), arg: 0, what: "the URL"},
			{rule: "ssrf-from-request", head: regexp.MustCompile(`(?:^|[^\w$.])https?\s*\.\s*(?:get|request)\s*\(`), arg: 0, what: "the URL"},
		},
	}
)

var jsReturnT = regexp.MustCompile(`(?:^|[^\w$.])return\s+(.*)$`)

// jsFindings follows request input inside the blocks of a JavaScript or
// TypeScript file. Handlers are often anonymous callbacks
// (app.get("/", (req, res) => {…})), so names are scoped by block depth
// instead of by declared function. cx is nil when the file is scanned on
// its own.
func (src *taintSource) jsFindings(cx *taintCtx) []finding {
	st := &taintState{lang: jsTaint, vars: map[string]taintVar{}}
	cx.attach(st)
	return src.jsScan(st, 1, len(src.code), nil)
}

// jsScan follows what st tracks through lines [from, to]. fn is the
// function being summarised, nil when reporting.
func (src *taintSource) jsScan(st *taintState, from, to int, fn *symbol) []finding {
	open := func(d int, l string) bool {
		t := strings.TrimSpace(l)
		return d > 0 && !strings.HasSuffix(t, "{") && !strings.HasSuffix(t, "=>")
	}
	var out []finding
	for _, s := range joinStatements(src.code, src.raw, from, to, open, nil) {
		d := src.depth[s.line-1]
		for n, v := range st.vars {
			if d < v.depth {
				delete(st.vars, n) // the block it was set in has closed
			}
		}
		owner := src.owners[s.line]
		st.class, st.decl = "", ""
		if owner != nil {
			st.class = owner.class
			if owner.start == s.line {
				st.decl = lastPart(owner.name) // its declaration is no call
			}
		}
		if fn == nil {
			st.entry = ownerName(owner)
			if st.entry == "" {
				st.entry = "a callback in " + src.rel
			}
		} else if s.line == fn.start {
			// An arrow function's expression body is what it returns.
			if _, body, ok := strings.Cut(s.code, "=>"); ok && !strings.HasPrefix(strings.TrimSpace(body), "{") {
				st.returns(body)
			}
		}
		if fn == nil && !strings.ContainsAny(s.code, "(=") || !st.relevant(s.code) {
			continue
		}
		found, hit := st.sinks(s, ownerName(owner), src.childProcess)
		out = append(out, found...)
		st.through(s.code)
		if fn != nil {
			if m := jsReturnT.FindStringSubmatch(s.code); m != nil {
				st.returns(m[1])
			}
		}
		m := jsAssignT.FindStringSubmatch(s.code)
		if m == nil || d == 0 {
			continue
		}
		names := jsNames(m[1])
		st.assign(names, m[3], m[2] == "+=", d)
		if hit {
			for _, n := range names {
				delete(st.vars, n)
			}
		}
	}
	return out
}

// jsNames returns the names a declaration binds: x, { a, b: c, ...d }, [e, f].
func jsNames(lhs string) []string {
	lhs = strings.TrimSpace(lhs)
	if !strings.HasPrefix(lhs, "{") && !strings.HasPrefix(lhs, "[") {
		return []string{lhs}
	}
	var out []string
	for _, part := range strings.Split(strings.Trim(lhs, "{}[] "), ",") {
		part, _, _ = strings.Cut(part, "=") // default value
		if _, alias, ok := strings.Cut(part, ":"); ok {
			part = alias
		}
		part = strings.TrimPrefix(strings.TrimSpace(part), "...")
		if identRe.FindString(part) == part && part != "" {
			out = append(out, part)
		}
	}
	return out
}
