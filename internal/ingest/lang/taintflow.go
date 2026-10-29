package lang

import (
	"math/bits"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Taint across the functions of one repository, for Python and
// JavaScript/TypeScript (security roadmap item 3, sixth step). Each
// function that some code calls gets a summary: which of its parameters
// reach which request-input sinks, and which reach its return value, and
// whether it returns request input itself. Summaries are built by scanning
// the function with its parameters marked, repeated until none changes
// (at most maxTaintRounds), and then every file is scanned once more with
// the summaries in hand: request input passed into a parameter that
// reaches a sink is reported at that sink, naming the function the input
// was read in and the calls it went through.
//
// Only calls the parser resolves through declarations and imports are
// followed (EvidenceSyntactic); a call resolved by a repository-unique
// name, or not at all, adds nothing.

const (
	maxTaintRounds = 8
	// maxTaintRefs caps the sinks one parameter's summary keeps.
	maxTaintRefs = 16
	// maxHops is how many calls a reported path may pass through.
	maxHops = 3
	// maxParams is how many parameters of a function are tracked.
	maxParams = 32
	// maxTaintFnsPerFile caps the functions of one file that get summaries.
	maxTaintFnsPerFile = 2000
	// maxTaintBytes caps the source kept after parsing for the second scan;
	// files past it keep their single-file findings.
	maxTaintBytes = 64 << 20
)

// taintSource is what a parser keeps of a file to scan it again.
type taintSource struct {
	rel     string
	raw     []string
	symbols []*symbol
	pattern []finding // the pattern findings, before taint superseded any

	// Python
	py    []pyLine
	pview []string // pyTaintView, made on first use

	// JavaScript and TypeScript
	code         []string // masked lines
	depth        []int
	owners       []*symbol // the innermost function of each line
	childProcess bool
}

func (src *taintSource) isPython() bool { return src.py != nil }

func (src *taintSource) view() []string {
	if src.pview == nil {
		src.pview = pyTaintView(src.raw)
	}
	return src.pview
}

func (src *taintSource) mentions(s string) bool {
	for _, l := range src.raw {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// size approximates the bytes the source holds.
func (src *taintSource) size() int {
	n := 0
	for _, l := range src.raw {
		n += len(l)
	}
	return 2 * n
}

// findings scans the whole file.
func (src *taintSource) findings(cx *taintCtx) []finding {
	if src.isPython() {
		return src.pyFindings(cx)
	}
	return src.jsFindings(cx)
}

// taintCtx is what a scan of a linked file uses to follow calls.
type taintCtx struct {
	resolve func(class, recv, name string) *taintFn
	cross   func(ref taintRef, path []string, from, entry string)
}

func (cx *taintCtx) attach(st *taintState) {
	if cx != nil {
		st.resolve, st.cross = cx.resolve, cx.cross
	}
}

// taintRef is a sink a parameter reaches: in the function itself (path
// empty) or through the calls on path.
type taintRef struct {
	rule, rel, owner, base string
	line                   int
	path                   []string
}

type taintSummary struct {
	sinks   [][]taintRef // by parameter
	returns uint64       // parameters that reach the return value
	from    string       // the request input it returns, if any
}

func (s *taintSummary) addSink(param int, r taintRef) {
	if param >= len(s.sinks) || len(s.sinks[param]) >= maxTaintRefs {
		return
	}
	for _, have := range s.sinks[param] {
		if have.rule == r.rule && have.rel == r.rel && have.line == r.line {
			return
		}
	}
	s.sinks[param] = append(s.sinks[param], r)
}

func (s *taintSummary) size() int {
	n := bits.OnesCount64(s.returns)
	for _, refs := range s.sinks {
		n += len(refs)
	}
	if s.from != "" {
		n++
	}
	return n
}

// taintFn is a function calls can reach.
type taintFn struct {
	name   string // as declared, "Class.method" for methods
	src    *taintSource
	sym    *symbol
	params []string
	npos   int // parameters filled by position, before the first "*"
	sum    taintSummary
}

func newTaintFn(src *taintSource, s *symbol) *taintFn {
	params := s.params
	if s.class != "" && len(params) > 0 && (params[0] == "self" || params[0] == "cls") {
		params = params[1:] // self.f(x) fills the second parameter
	}
	params = params[:min(len(params), maxParams)]
	npos := len(params)
	if i := slices.Index(params, "*"); i >= 0 {
		npos = i
	}
	return &taintFn{name: s.name, src: src, sym: s, params: params, npos: npos, sum: taintSummary{sinks: make([][]taintRef, len(params))}}
}

type boundArg struct {
	param int
	arg   string
}

// bind pairs the arguments of a call, whose text starts after the opening
// parenthesis, with the parameters they fill. Binding stops at a spread
// (*args, **kw, ...xs): what follows lands in unknown places.
func (fn *taintFn) bind(args string) []boundArg {
	var out []boundArg
	for i, a := range splitArgs(args) {
		a = strings.TrimSpace(a)
		switch {
		case a == "":
			continue
		case strings.HasPrefix(a, "*") || strings.HasPrefix(a, "..."):
			return out
		case fn.src.isPython() && isKeywordArg(a):
			name, val, _ := strings.Cut(a, "=")
			if p := slices.Index(fn.params, strings.TrimSpace(name)); p >= 0 {
				out = append(out, boundArg{p, val})
			}
		case i < fn.npos:
			out = append(out, boundArg{i, a})
		}
	}
	return out
}

// callee is the repository function the call head at m in code names, or
// nil: a method of some other value, a declaration, or a call the parser
// did not resolve through declarations and imports.
func (st *taintState) callee(code string, m []int) *taintFn {
	if st.resolve == nil || m[0] > 0 && (code[m[0]-1] == '.' || isIdentByte(code[m[0]-1])) {
		return nil
	}
	before := strings.TrimRight(code[:m[0]], " \t*")
	if strings.HasSuffix(before, "function") || strings.HasSuffix(before, "def") {
		return nil
	}
	recv, name, dotted := strings.Cut(callName(code[m[0]:m[1]]), ".")
	if !dotted {
		recv, name = "", recv
		if name == st.decl {
			return nil
		}
	} else if strings.Contains(name, ".") {
		return nil
	}
	return st.resolve(st.class, recv, name)
}

// returned is the taint of a call's result under the callee's summary:
// the request input it returns, and the taint of the arguments that reach
// its return value.
func (st *taintState) returned(fn *taintFn, args string, nest int) taint {
	t := taint{from: fn.sum.from}
	for _, b := range fn.bind(args) {
		if fn.sum.returns&(1<<b.param) != 0 {
			t = t.or(st.taintAt(b.arg, nest))
		}
	}
	return t
}

// returns records what a returned expression carries in the summary
// being built.
func (st *taintState) returns(expr string) {
	if st.fn == nil {
		return
	}
	t := st.taintOf(expr)
	st.fn.sum.returns |= t.params
	if t.from != "" && st.fn.sum.from == "" {
		st.fn.sum.from = t.from
		if !strings.Contains(t.from, ", returned by ") {
			st.fn.sum.from += ", returned by " + st.fn.name
		}
	}
}

// through applies the summaries of the repository functions a statement
// calls: a tainted parameter passed on adds the callee's sinks to the
// summary being built, request input reports them.
func (st *taintState) through(code string) {
	if st.resolve == nil {
		return
	}
	for _, m := range callHead.FindAllStringIndex(code, -1) {
		fn := st.callee(code, m)
		if fn == nil || fn.sum.size() == 0 {
			continue
		}
		for _, b := range fn.bind(code[m[1]:]) {
			refs := fn.sum.sinks[b.param]
			if len(refs) == 0 {
				continue
			}
			t := st.taintOf(b.arg)
			if t.none() {
				continue
			}
			for _, ref := range refs {
				path := append([]string{fn.name}, ref.path...)
				if st.fn != nil {
					if len(path) >= maxHops {
						continue // whoever calls this adds one more
					}
					for i := range maxParams {
						if t.params&(1<<i) != 0 {
							r := ref
							r.path = path
							st.fn.sum.addSink(i, r)
						}
					}
					continue
				}
				if t.from != "" && st.cross != nil && len(path) <= maxHops {
					st.cross(ref, path, t.from, st.entry)
				}
			}
		}
	}
}

// summarise scans fn with its parameters marked and says whether its
// summary grew.
func (fn *taintFn) summarise(cx *taintCtx) bool {
	before := fn.sum.size()
	src, s := fn.src, fn.sym
	lang := jsTaint
	if src.isPython() {
		lang = pyTaint
	}
	st := &taintState{lang: lang, vars: map[string]taintVar{}, class: s.class, fn: fn}
	cx.attach(st)
	st.cross = nil
	for i, p := range fn.params {
		if p != "" && p != "*" {
			st.vars[p] = taintVar{t: taint{params: 1 << i}}
		}
	}
	if src.isPython() {
		src.pyScan(st, s)
	} else {
		src.jsScan(st, s.start, min(s.end, len(src.code)), s)
	}
	return fn.sum.size() != before
}

var (
	jsArrowParam = regexp.MustCompile(`=\s*(?:async\s+)?([A-Za-z_$][\w$]*)\s*=>`)
	jsParamMods  = regexp.MustCompile(`^(?:(?:public|private|protected|readonly|override)\s+)+`)
)

// jsParams returns the parameter names of a JavaScript or TypeScript
// function from its masked lines: "*" for a rest parameter, "" for a
// destructured one; defaults, types and optional marks dropped.
func jsParams(lines []string, s *symbol) []string {
	var sig strings.Builder
	for i := s.start; i <= len(lines) && i < s.start+maxJoin; i++ {
		sig.WriteString(lines[i-1] + " ")
		if strings.Contains(lines[i-1], ")") || strings.Contains(lines[i-1], "=>") {
			break
		}
	}
	text := sig.String()
	if m := jsArrowParam.FindStringSubmatch(text); m != nil {
		return []string{m[1]} // const f = x => …
	}
	name := lastPart(s.name)
	at := 0
	if i := strings.Index(text, name); i >= 0 {
		at = i + len(name)
	}
	open := strings.IndexByte(text[at:], '(')
	if open < 0 {
		return nil
	}
	var out []string
	for _, p := range splitArgs(text[at+open+1:]) {
		p = jsParamMods.ReplaceAllString(strings.TrimSpace(p), "")
		switch {
		case p == "":
			continue
		case strings.HasPrefix(p, "..."):
			out = append(out, "*")
		case p[0] == '{' || p[0] == '[':
			out = append(out, "")
		default:
			n := identRe.FindString(p)
			if n == "this" {
				continue // a TypeScript this-parameter takes no argument
			}
			out = append(out, n)
		}
	}
	return out
}

var (
	djangoGroup   = regexp.MustCompile(`\(\?P<(\w+)>`)
	djangoNumeric = regexp.MustCompile(`^(?:\\d|\[0-9\])(?:[+*]|\{\d+(?:,\d*)?\})?$`)
)

// djangoParams returns the URL parameters of a Django route pattern a view
// receives as arguments: <name> and <str:name> or <path:name>, and the
// named groups of a re_path. Integer and UUID converters and groups of
// digits only are clean.
func djangoParams(pattern string) []string {
	var out []string
	rest := pattern
	for {
		m := djangoGroup.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}
		name := rest[m[2]:m[3]]
		end := closingParen(rest, m[1])
		body := rest[m[1]:max(m[1], end-1)]
		if !djangoNumeric.MatchString(body) {
			out = append(out, name)
		}
		rest = rest[:m[0]] + rest[min(end, len(rest)):]
	}
	for _, p := range pyRouteParam.FindAllStringSubmatch(rest, -1) {
		switch {
		case p[2] == "":
		case p[1] == "int" || p[1] == "uuid":
		default:
			out = append(out, p[2])
		}
	}
	return out
}

// addInputs makes the parameters of s named in names client input.
func (s *symbol) addInputs(names []string) {
	for _, n := range names {
		if slices.Contains(s.params, n) && !slices.Contains(s.inputs, n) {
			s.inputs = append(s.inputs, n)
		}
	}
}

// taintAcross summarises the functions calls reach and scans every file
// again with the summaries: findings in callees reached from request
// input, and the more precise results of calls into the repository.
// resolvers resolve a call in each file; called holds the node IDs calls
// reach through declarations and imports.
func (ix *indexer) taintAcross(resolvers map[*parsedFile]func(call, string) (string, model.Evidence), called map[string]bool) {
	rels := slices.Sorted(func(yield func(string) bool) {
		for rel, pf := range ix.files {
			if pf.fr.taint != nil && !yield(rel) {
				return
			}
		}
	})
	if len(rels) == 0 {
		return
	}
	fns := map[string]*taintFn{}
	var order []*taintFn
	for _, rel := range rels {
		pf := ix.files[rel]
		pf.fr.taint.rel = rel
		n := 0
		for _, s := range pf.fr.symbols {
			sid := pf.symIDs[s.name]
			if s.kind == model.KindType || !called[sid] || n >= maxTaintFnsPerFile {
				continue
			}
			fn := newTaintFn(pf.fr.taint, s)
			fns[sid] = fn
			order = append(order, fn)
			n++
		}
	}
	ctxOf := func(pf *parsedFile, cross func(taintRef, []string, string, string)) *taintCtx {
		resolve := resolvers[pf]
		return &taintCtx{
			resolve: func(class, recv, name string) *taintFn {
				sid, ev := resolve(call{recv: recv, name: name}, class)
				if ev != model.EvidenceSyntactic {
					return nil
				}
				return fns[sid]
			},
			cross: cross,
		}
	}
	ctxs := map[*taintSource]*taintCtx{}
	for _, rel := range rels {
		pf := ix.files[rel]
		ctxs[pf.fr.taint] = ctxOf(pf, nil)
	}
	for range maxTaintRounds {
		changed := false
		for _, fn := range order {
			changed = fn.summarise(ctxs[fn.src]) || changed
		}
		if !changed {
			break
		}
	}

	found := map[string][]finding{}
	for _, rel := range rels {
		pf := ix.files[rel]
		cross := func(ref taintRef, path []string, from, entry string) {
			detail := ref.base + " (via " + from + " in " + entry + ", through " + strings.Join(path, " → ") + ")"
			found[ref.rel] = append(found[ref.rel], finding{rule: ref.rule, detail: detail, line: ref.line, owner: ref.owner})
		}
		own := pf.fr.taint.findings(ctxOf(pf, cross))
		// The file's own findings come first: where a line holds both, the
		// one read in the same function is the clearer report.
		found[rel] = append(own, found[rel]...)
	}
	for _, rel := range rels {
		src := ix.files[rel].fr.taint
		ix.files[rel].fr.findings = supersede(src.pattern, found[rel])
	}
}
