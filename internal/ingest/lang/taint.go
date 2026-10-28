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
//     through attributes, containers, calls into other functions or
//     returns.
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

// taintVar is a tainted local and the depth of the block it was set in.
type taintVar struct {
	from  string
	depth int
}

type taintState struct {
	lang *taintLang
	vars map[string]taintVar
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

// relevant is a cheap test that a statement can matter: some name is
// tainted already, or it mentions a request source.
func (st *taintState) relevant(code string) bool {
	if len(st.vars) > 0 {
		return true
	}
	for _, l := range st.lang.guards {
		if strings.Contains(code, l) {
			return st.lang.sources.MatchString(code)
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

// taintOf returns the request input expr carries, or "".
func (st *taintState) taintOf(expr string) string {
	expr = blankSanitized(expr, st.lang.sanitizers)
	if strings.Contains(expr, "==") || strings.Contains(expr, "!=") {
		if !strings.Contains(expr, "?") && !strings.Contains(expr, " if ") {
			return "" // a comparison is a boolean
		}
	}
	for _, m := range st.lang.sources.FindAllStringIndex(expr, -1) {
		if m[0] > 0 && (isIdentByte(expr[m[0]-1]) || expr[m[0]-1] == '.' && !strings.HasPrefix(expr[m[0]:], "request.")) {
			continue
		}
		if st.lang.skipSource != nil && st.lang.skipSource(expr, m[1]) {
			continue
		}
		return strings.Join(strings.Fields(expr[m[0]:m[1]]), "")
	}
	if len(st.vars) == 0 {
		return ""
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
		return v.from
	}
	return ""
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// blankSanitized replaces calls to sanitizers, arguments included, with
// spaces: int(request.args["n"]) carries nothing.
func blankSanitized(expr string, sanitizers map[string]bool) string {
	var b []byte
	for _, m := range callHead.FindAllStringIndex(expr, -1) {
		if m[0] > 0 && expr[m[0]-1] == '.' {
			continue
		}
		name := strings.Join(strings.Fields(strings.TrimSuffix(expr[m[0]:m[1]], "(")), "")
		if !sanitizers[name] {
			continue
		}
		if b == nil {
			b = []byte(expr)
		}
		end := closingParen(expr, m[1])
		for i := m[0]; i < end; i++ {
			b[i] = ' '
		}
	}
	if b == nil {
		return expr
	}
	return string(b)
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

// sinks reports the sinks of one statement that receive request input.
func (st *taintState) sinks(s stmt, owner string, childProcess bool) []finding {
	var out []finding
	done := map[string]bool{}
	for _, sk := range st.lang.sinks {
		if done[sk.rule] || sk.childProcess && !childProcess {
			continue
		}
		for _, m := range sk.head.FindAllStringIndex(s.code, -1) {
			from := st.sinkArg(sk, s, m[1])
			if from == "" {
				continue
			}
			out = append(out, finding{rule: sk.rule, detail: calleeAt(s.code, m[1]) + ": " + sk.what + " comes from the incoming request (via " + from + ")", line: s.line, owner: owner})
			done[sk.rule] = true
			break
		}
	}
	return out
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

// sinkArg returns what request input reaches the checked argument of the
// call whose arguments start at off.
func (st *taintState) sinkArg(sk taintSink, s stmt, off int) string {
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
	for _, sp := range picked {
		a := strings.TrimSpace(s.code[off+sp[0] : off+sp[1]])
		if a == "" || a[0] == '{' || a[0] == '[' && sk.rule == "sql-from-request" {
			continue // an options object or a parameter list, not the text
		}
		if sk.rule == "ssrf-from-request" && fixedURL.MatchString(strings.TrimSpace(s.raw[off+sp[0]:min(len(s.raw), off+sp[1])])) {
			continue // the client chooses a path on a fixed host at most
		}
		if from := st.taintOf(a); from != "" {
			return from
		}
	}
	return ""
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
	from := st.taintOf(rhs)
	for _, n := range names {
		switch {
		case from != "":
			st.vars[n] = taintVar{from: from, depth: depth}
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
	var out []string
	for _, p := range splitArgs(s[open+1:]) {
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

// pyTaintFindings follows request input inside each function of a parsed
// Python file.
func pyTaintFindings(fr *fileResult, raw []string, lines []pyLine) []finding {
	var funcs []*symbol
	for _, s := range fr.symbols {
		if s.kind != model.KindType && s.end >= s.start {
			funcs = append(funcs, s)
		}
	}
	if len(funcs) == 0 || !strings.Contains(strings.Join(raw, "\n"), "request") && !hasInputs(funcs) {
		return nil
	}
	view := pyTaintView(raw)
	var out []finding
	for _, s := range funcs {
		st := &taintState{lang: pyTaint, vars: map[string]taintVar{}}
		for _, p := range s.inputs {
			st.vars[p] = taintVar{from: "route parameter " + p}
		}
		open := func(depth int, l string) bool {
			return depth > 0 || strings.HasSuffix(strings.TrimSpace(l), "\\")
		}
		skip := func(i int) bool { return lines[i-1].blank }
		for _, stm := range joinStatements(view, raw, s.start, s.end, open, skip) {
			t := strings.TrimSpace(stm.code)
			if strings.HasPrefix(t, "@") || strings.HasPrefix(t, "def ") || strings.HasPrefix(t, "async def ") || strings.HasPrefix(t, "class ") {
				continue
			}
			if !st.relevant(stm.code) {
				continue
			}
			found := st.sinks(stm, s.name, false)
			out = append(out, found...)
			if m := pyForT.FindStringSubmatch(stm.code); m != nil {
				st.assign(splitNames(m[1]), m[2], false, 0)
			} else if m := pyAssignT.FindStringSubmatch(stm.code); m != nil {
				names := splitNames(m[1])
				st.assign(names, m[3], m[2] == "+=", 0)
				if len(found) > 0 {
					for _, n := range names {
						delete(st.vars, n) // reported at the sink already
					}
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

// jsTaintFindings follows request input inside the blocks of a JavaScript
// or TypeScript file. Handlers are often anonymous callbacks
// (app.get("/", (req, res) => {…})), so names are scoped by block depth
// instead of by declared function.
func jsTaintFindings(raw, lines []string, depth []int, owners []*symbol, childProcess bool) []finding {
	st := &taintState{lang: jsTaint, vars: map[string]taintVar{}}
	open := func(d int, l string) bool {
		t := strings.TrimSpace(l)
		return d > 0 && !strings.HasSuffix(t, "{") && !strings.HasSuffix(t, "=>")
	}
	var out []finding
	for _, s := range joinStatements(lines, raw, 1, len(lines), open, nil) {
		d := depth[s.line-1]
		for n, v := range st.vars {
			if d < v.depth {
				delete(st.vars, n) // the block it was set in has closed
			}
		}
		if !strings.ContainsAny(s.code, "(=") || !st.relevant(s.code) {
			continue
		}
		found := st.sinks(s, ownerName(owners[s.line]), childProcess)
		out = append(out, found...)
		m := jsAssignT.FindStringSubmatch(s.code)
		if m == nil || d == 0 {
			continue
		}
		names := jsNames(m[1])
		st.assign(names, m[3], m[2] == "+=", d)
		if len(found) > 0 {
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
