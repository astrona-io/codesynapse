package golang

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// Taint across the repository's own functions (security roadmap item 3,
// second step). Every function gets a summary: which of its parameters
// reach which request-input sinks, and which reach its results. Summaries
// are computed by scanning each function with its parameters marked,
// repeated until none changes (calls between functions feed each other),
// and then every function is scanned once more with the summaries in hand:
// request input passed into a parameter that reaches a sink is reported at
// that sink, naming the caller and the call it went through; a result
// computed from request input is tainted at the call site.
//
// Static calls to the repository's functions are followed, and calls
// through the repository's interfaces apply the summaries of every
// implementation in the repository. With --dataflow, calls through function
// values (closures, func-typed parameters and fields, method values) apply
// the summaries of every repository function or closure VTA says can reach
// the call; without it, or for a value VTA resolved nowhere, they add
// nothing.

const (
	maxTaintRounds    = 8
	maxSinksPerParam  = 16
	paramTaintPrefix  = "\x00param:"
	reachedSinkDetail = "%s (via %s in %s, passed to %s)"
)

// sinkRef is a sink inside a function, reached from one of its parameters.
type sinkRef struct {
	rule, rel, ownerID, detail string
	line                       int
}

// summary is what a function does with its parameters.
type summary struct {
	params  int
	sinks   [][]sinkRef // by parameter
	returns []bool      // by parameter: the parameter reaches a result
}

func newSummary(sig *types.Signature) *summary {
	n := sig.Params().Len()
	return &summary{params: n, sinks: make([][]sinkRef, n), returns: make([]bool, n)}
}

// param maps a call argument to the parameter it fills (variadic
// arguments all fill the last one), or -1.
func (s *summary) param(arg int) int {
	switch {
	case s.params == 0:
		return -1
	case arg >= s.params:
		return s.params - 1
	}
	return arg
}

func (s *summary) size() int {
	n := 0
	for i := range s.sinks {
		n += len(s.sinks[i])
		if s.returns[i] {
			n++
		}
	}
	return n
}

func (s *summary) addSink(param int, r sinkRef) {
	if param < 0 || param >= s.params || len(s.sinks[param]) >= maxSinksPerParam {
		return
	}
	for _, have := range s.sinks[param] {
		if have.rule == r.rule && have.rel == r.rel && have.line == r.line {
			return
		}
	}
	s.sinks[param] = append(s.sinks[param], r)
}

func paramTaint(i int) string { return paramTaintPrefix + strconv.Itoa(i) }

// paramOf is the parameter a taint marks, or -1 for request input.
func paramOf(taint string) int {
	if !strings.HasPrefix(taint, paramTaintPrefix) {
		return -1
	}
	i, err := strconv.Atoi(strings.TrimPrefix(taint, paramTaintPrefix))
	if err != nil {
		return -1
	}
	return i
}

// sink is where a request-input rule fires: with request input it reports,
// with a marked parameter (while summarising) it records the sink in the
// function's summary.
func (s *scanner) sink(rule string, pos token.Pos, detail, from string) {
	if i := paramOf(from); i >= 0 {
		if s.sum != nil {
			s.sum.addSink(i, sinkRef{rule: rule, rel: s.rel, ownerID: s.ownerID, detail: detail, line: s.p.fset.Position(pos).Line})
		}
		return
	}
	s.report(rule, pos, withTaint(detail, from))
}

// through applies a callee's summary at a call: a marked parameter passed
// on adds the callee's sinks to this function's summary, request input
// reports them.
func (s *scanner) through(call *ast.CallExpr, callee string, sum *summary) {
	for j, a := range call.Args {
		from := s.taintOf(a)
		if from == "" {
			continue
		}
		p := sum.param(j)
		if p < 0 {
			continue
		}
		for _, ref := range sum.sinks[p] {
			if i := paramOf(from); i >= 0 {
				if s.sum != nil {
					s.sum.addSink(i, ref)
				}
				continue
			}
			if !s.quiet {
				s.x.addFinding(ref.rel, ref.line, ref.ownerID, ref.rule, fmt.Sprintf(reachedSinkDetail, ref.detail, from, s.name, callee))
			}
		}
	}
}

// summariesOf are the summaries a call to fn applies: fn's own, or for an
// interface method those of its implementations.
func (s *scanner) summariesOf(fn *types.Func) []*summary {
	if sum := s.sums[fn]; sum != nil {
		return []*summary{sum}
	}
	var out []*summary
	for _, impl := range s.x.dispatch[fn] {
		if sum := s.sums[impl]; sum != nil {
			out = append(out, sum)
		}
	}
	return out
}

// dynamicSummaries are the summaries a call through a function value
// applies: those of the targets dataflow resolved for it.
func (s *scanner) dynamicSummaries(call *ast.CallExpr) []*summary {
	var out []*summary
	for _, t := range s.x.dynamic[call.Lparen] {
		sum := s.sums[t.fn]
		if t.lit != nil {
			sum = s.lits[t.lit]
		}
		if sum != nil {
			out = append(out, sum)
		}
	}
	return out
}

// returnsTaint is the taint of a call's result under the callee's summary.
func (s *scanner) returnsTaint(call *ast.CallExpr, sum *summary) string {
	for j, a := range call.Args {
		if p := sum.param(j); p >= 0 && sum.returns[p] {
			if from := s.taintOf(a); from != "" {
				return from
			}
		}
	}
	return ""
}

// taintFunc is a function kept for the interprocedural pass.
type taintFunc struct {
	fn      *types.Func
	decl    *ast.FuncDecl
	info    *types.Info
	fset    *token.FileSet
	ownerID string
	rel     string
}

// taintLit is a closure some call through a function value reaches. It is
// summarised in the context of the declared function it belongs to, which
// owns the findings inside it.
type taintLit struct {
	lit  *ast.FuncLit
	sig  *types.Signature
	encl *taintFunc
}

// interprocedural computes the summaries and reports what they reveal.
func (x *extractor) interprocedural() {
	if len(x.taintFuncs) == 0 {
		return
	}
	sums := make(map[*types.Func]*summary, len(x.taintFuncs))
	for _, f := range x.taintFuncs {
		sums[f.fn] = newSummary(f.fn.Signature())
	}
	lits := make(map[*ast.FuncLit]*summary, len(x.taintLits))
	for _, l := range x.taintLits {
		lits[l.lit] = newSummary(l.sig)
	}
	// learn scans one body with its parameters marked and reports whether
	// the summary grew.
	learn := func(sum *summary, f *taintFunc, params *ast.FieldList, body *ast.BlockStmt) bool {
		before := sum.size()
		s := x.newScanner(f.info, f.fset, f.ownerID, f.rel)
		s.quiet, s.sum, s.sums, s.lits, s.name = true, sum, sums, lits, f.fn.Name()
		i := 0
		for _, field := range params.List {
			for _, name := range field.Names {
				if obj := f.info.Defs[name]; obj != nil {
					s.taint[obj] = paramTaint(i)
				}
				i++
			}
			if len(field.Names) == 0 {
				i++
			}
		}
		s.walk(body)
		return sum.size() != before
	}
	for range maxTaintRounds {
		changed := false
		for _, f := range x.taintFuncs {
			changed = learn(sums[f.fn], f, f.decl.Type.Params, f.decl.Body) || changed
		}
		for _, l := range x.taintLits {
			changed = learn(lits[l.lit], l.encl, l.lit.Type.Params, l.lit.Body) || changed
		}
		if !changed {
			break
		}
	}
	// Closures are walked as part of the function they belong to.
	for _, f := range x.taintFuncs {
		s := x.newScanner(f.info, f.fset, f.ownerID, f.rel)
		s.sums, s.lits, s.name = sums, lits, f.fn.Name()
		s.walk(f.decl.Body)
	}
}
