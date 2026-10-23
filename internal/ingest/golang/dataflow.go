package golang

import (
	"context"
	"fmt"
	"go/types"
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
