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
// Only static calls to the repository's functions are followed; calls
// through interfaces and function values are not (yet).

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
func (s *scanner) through(call *ast.CallExpr, fn *types.Func, sum *summary) {
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
				s.x.addFinding(ref.rel, ref.line, ref.ownerID, ref.rule, fmt.Sprintf(reachedSinkDetail, ref.detail, from, s.name, fn.Name()))
			}
		}
	}
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

// interprocedural computes the summaries and reports what they reveal.
func (x *extractor) interprocedural() {
	if len(x.taintFuncs) == 0 {
		return
	}
	sums := make(map[*types.Func]*summary, len(x.taintFuncs))
	for _, f := range x.taintFuncs {
		sums[f.fn] = newSummary(f.fn.Signature())
	}
	for range maxTaintRounds {
		changed := false
		for _, f := range x.taintFuncs {
			sum := sums[f.fn]
			before := sum.size()
			s := x.newScanner(f.info, f.fset, f.ownerID, f.rel)
			s.quiet, s.sum, s.sums, s.name = true, sum, sums, f.fn.Name()
			i := 0
			for _, field := range f.decl.Type.Params.List {
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
			s.walk(f.decl.Body)
			changed = changed || sum.size() != before
		}
		if !changed {
			break
		}
	}
	for _, f := range x.taintFuncs {
		s := x.newScanner(f.info, f.fset, f.ownerID, f.rel)
		s.sums, s.name = sums, f.fn.Name()
		s.walk(f.decl.Body)
	}
}
