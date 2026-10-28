package golang

import (
	"cmp"
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// maxDataflowFunctions bounds the SSA functions (declared functions,
// closures and generic instances) handed to VTA, which cannot be cancelled
// once it starts. Larger repositories skip the analysis and say so.
const maxDataflowFunctions = 200_000

// dataflow adds CALLS edges, with dataflow evidence, for calls through
// function values that the typed pass counted as unresolved. It builds SSA
// for the well-typed primary packages already loaded (no further `go list`)
// and asks variable type analysis (VTA) which repository functions can
// reach each such call site. Static and interface calls keep their typed
// edges. Packages with errors, and everything when the analysis is too
// large or fails, are marked skipped; the typed graph is kept either way.
// Only cancellation is returned as an error.
func (x *extractor) dataflow(ctx context.Context, primary []*packages.Package) error {
	var wellTyped []*packages.Package
	for _, p := range primary {
		if !x.pkgInRepo(p) {
			continue
		}
		if len(p.Errors) > 0 || p.Types == nil {
			x.pkgAnalysis(p).Dataflow = model.DataflowSkipped
			continue
		}
		wellTyped = append(wellTyped, p)
	}
	if len(wellTyped) == 0 {
		return nil
	}
	skip := func(reason string, args ...any) {
		logx.FromContext(ctx).Warn("skipping data-flow call analysis", append([]any{"reason", reason}, args...)...)
		for _, p := range wellTyped {
			x.pkgAnalysis(p).Dataflow = model.DataflowSkipped
		}
	}
	limit := x.opts.MaxDataflowFunctions
	if limit <= 0 {
		limit = maxDataflowFunctions
	}
	cg, err := buildVTA(ctx, wellTyped, limit)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		skip(err.Error())
		return nil
	}
	for _, p := range wellTyped {
		x.pkgAnalysis(p).Dataflow = model.DataflowVTA
	}
	x.recordDynamic(cg)

	// SSA turns some calls the type checker could not follow (fn := f;
	// fn()) into static calls, so static sites are kept too: a site counts
	// when it adds an edge the typed pass did not find.
	typed := map[[2]string]bool{}
	for _, e := range x.g.Edges() {
		if e.Kind == model.EdgeCalls && e.Evidence == model.EvidenceTyped {
			typed[[2]string{e.From, e.To}] = true
		}
	}
	sites := map[string]map[ssa.CallInstruction]bool{} // caller node ID -> linked sites
	for fn, node := range cg.Nodes {
		if fn == nil {
			continue
		}
		callerID := x.ssaFuncID(enclosing(fn))
		if callerID == "" {
			continue
		}
		for _, e := range node.Out {
			if e.Site.Common().IsInvoke() || e.Callee.Func.Parent() != nil {
				continue // DISPATCHES_TO covers interfaces; closures have no node of their own
			}
			calleeID := x.ssaFuncID(e.Callee.Func)
			if calleeID == "" || calleeID == callerID || typed[[2]string{callerID, calleeID}] {
				continue
			}
			x.g.AddEdge(model.Edge{From: callerID, To: calleeID, Kind: model.EdgeCalls, Evidence: model.EvidenceDataflow})
			if sites[callerID] == nil {
				sites[callerID] = map[ssa.CallInstruction]bool{}
			}
			sites[callerID][e.Site] = true
		}
	}
	for callerID, linked := range sites {
		n, _ := x.g.Node(callerID)
		a, err := model.DecodeAnalysis(n.Analysis)
		if err != nil {
			continue // written by funcDecl in this process
		}
		a.Level, a.DataflowCalls = model.LevelTyped, len(linked)
		if a.Status == "" {
			a.Status = model.StatusComplete
		}
		n.Analysis = a.Encode()
		if pa, ok := x.analysis[x.id(model.KindPackage, n.Package)]; ok {
			pa.DataflowCalls += len(linked)
		}
	}
	return nil
}

// buildVTA builds SSA for pkgs and runs VTA over their functions. x/tools
// documents VTA as experimental, so a panic inside it becomes an error that
// skips the analysis instead of failing ingest.
func buildVTA(ctx context.Context, pkgs []*packages.Package, maxFuncs int) (cg *callgraph.Graph, err error) {
	defer func() {
		if r := recover(); r != nil {
			cg, err = nil, fmt.Errorf("analysis failed: %v", r)
		}
	}()
	prog, built := ssautil.Packages(pkgs, ssa.InstantiateGenerics)
	repo := map[*types.Package]bool{}
	for _, sp := range built {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if sp != nil {
			sp.Build()
			repo[sp.Pkg] = true
		}
	}
	funcs := map[*ssa.Function]bool{}
	for fn := range ssautil.AllFunctions(prog) {
		pkg := fn.Pkg
		if pkg == nil && fn.Origin() != nil {
			pkg = fn.Origin().Pkg
		}
		if pkg != nil && repo[pkg.Pkg] {
			funcs[fn] = true
		}
	}
	if len(funcs) > maxFuncs {
		return nil, fmt.Errorf("%d functions exceed the limit of %d", len(funcs), maxFuncs)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return vta.CallGraph(funcs, nil), nil
}

// enclosing returns the declared function a closure belongs to.
func enclosing(fn *ssa.Function) *ssa.Function {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	return fn
}

// ssaFuncID maps an SSA function to the node ID of the repository function
// or method it was declared as, or "" for synthetic package initialisers,
// functions outside the repository and functions the graph does not hold.
func (x *extractor) ssaFuncID(fn *ssa.Function) string {
	if origin := fn.Origin(); origin != nil {
		fn = origin
	}
	obj, ok := fn.Object().(*types.Func)
	if !ok || obj.Pkg() == nil || !x.inRepo(strings.TrimSuffix(obj.Pkg().Path(), "_test")) {
		return ""
	}
	id := x.funcID(obj.Origin())
	if _, ok := x.g.Node(id); !ok {
		return ""
	}
	return id
}

// maxDynamicTargets bounds the targets kept for one call through a function
// value; the first ones in source order are kept.
const maxDynamicTargets = 64

// dynTarget is what a call through a function value can reach: a declared
// function or method of the repository (fn), or a closure in one (lit). at
// orders targets by source position, which unlike token.Pos does not depend
// on the order files were parsed in.
type dynTarget struct {
	fn  *types.Func
	lit *ast.FuncLit
	at  string
}

// recordDynamic keeps, for every call site VTA resolved that is not an
// interface call, the repository functions and closures it can reach, so
// the taint pass (interprocedural.go) can apply their summaries. Targets
// outside the repository, in test files, or synthetic wrappers whose
// parameters do not line up with the method's (method expressions) are
// left out: a value with no kept target adds nothing.
func (x *extractor) recordDynamic(cg *callgraph.Graph) {
	byFunc := make(map[*types.Func]*taintFunc, len(x.taintFuncs))
	for _, f := range x.taintFuncs {
		byFunc[f.fn] = f
	}
	lits := map[*ast.FuncLit]*taintLit{}
	sites := map[token.Pos]map[dynTarget]bool{}
	for fn, node := range cg.Nodes {
		if fn == nil {
			continue
		}
		for _, e := range node.Out {
			c := e.Site.Common()
			if c.IsInvoke() || !c.Pos().IsValid() {
				continue
			}
			t, ok := dynTargetOf(e.Callee.Func, byFunc, lits)
			if !ok {
				continue
			}
			if sites[c.Pos()] == nil {
				sites[c.Pos()] = map[dynTarget]bool{}
			}
			sites[c.Pos()][t] = true
		}
	}
	x.dynamic = make(map[token.Pos][]dynTarget, len(sites))
	for pos, set := range sites {
		ts := make([]dynTarget, 0, len(set))
		for t := range set {
			ts = append(ts, t)
		}
		slices.SortFunc(ts, func(a, b dynTarget) int { return cmp.Compare(a.at, b.at) })
		if len(ts) > maxDynamicTargets {
			ts = ts[:maxDynamicTargets]
		}
		x.dynamic[pos] = ts
	}
	x.taintLits = x.taintLits[:0]
	for _, l := range lits {
		x.taintLits = append(x.taintLits, l)
	}
	slices.SortFunc(x.taintLits, func(a, b *taintLit) int {
		return cmp.Compare(sourceAt(a.encl, a.lit.Pos()), sourceAt(b.encl, b.lit.Pos()))
	})
}

// dynTargetOf maps a VTA callee to the function or closure the taint pass
// holds a summary for, adding closures to lits as they are met.
func dynTargetOf(fn *ssa.Function, byFunc map[*types.Func]*taintFunc, lits map[*ast.FuncLit]*taintLit) (dynTarget, bool) {
	if fn.Parent() != nil {
		lit, ok := fn.Syntax().(*ast.FuncLit)
		encl := byFunc[declaredFunc(enclosing(fn))]
		if !ok || encl == nil {
			return dynTarget{}, false
		}
		if lits[lit] == nil {
			sig, ok := encl.info.TypeOf(lit).(*types.Signature)
			if !ok {
				return dynTarget{}, false
			}
			lits[lit] = &taintLit{lit: lit, sig: sig, encl: encl}
		}
		return dynTarget{lit: lit, at: sourceAt(encl, lit.Pos())}, true
	}
	obj := declaredFunc(fn)
	f := byFunc[obj]
	if f == nil || fn.Signature.Params().Len() != obj.Signature().Params().Len() {
		return dynTarget{}, false // a method expression takes the receiver first
	}
	return dynTarget{fn: obj, at: sourceAt(f, f.decl.Pos())}, true
}

// declaredFunc is the declared (generic origin) function an SSA function,
// instance or method wrapper stands for, or nil.
func declaredFunc(fn *ssa.Function) *types.Func {
	if origin := fn.Origin(); origin != nil {
		fn = origin
	}
	obj, ok := fn.Object().(*types.Func)
	if !ok {
		return nil
	}
	return obj.Origin()
}

// sourceAt is a sortable "file:offset" for pos inside f's file.
func sourceAt(f *taintFunc, pos token.Pos) string {
	p := f.fset.Position(pos)
	return fmt.Sprintf("%s:%010d", f.rel, p.Offset)
}
