package golang

import (
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Entry points (docs/PLAN.md, security roadmap item 4 and agent workflow
// roadmap step 12): HTTP routes, MCP tools and CLI commands become Route
// nodes with a HANDLED_BY edge to the function that serves them, so "which
// endpoints reach exec" and "which endpoint ends up here" are graph walks.
//
// Registration is recognised by shape, so wrappers count: a constant route
// pattern ("GET /x", "/x") passed with a function to a known registration
// method (HandleFunc, Get, POST, …) or to a function of the repository
// itself (s.route("GET /x", policy, h)); an AddTool call; a cobra.Command
// literal with Run or RunE. The other arguments of the registration and the
// functions wrapped around the handler are kept as its guards. Middleware
// applied to a whole router (r.Use(auth)) is not seen.

var routePatternRe = regexp.MustCompile(`^(?:(GET|HEAD|POST|PUT|PATCH|DELETE|CONNECT|OPTIONS|TRACE)\s+)?([A-Za-z0-9.-]*)(/\S*)$`)

// routeMethods maps registration method names to the HTTP method they
// imply ("" for any): net/http, chi, gin, echo, gorilla/mux and fiber.
var routeMethods = map[string]string{
	"Handle": "", "HandleFunc": "", "Any": "", "Match": "", "Method": "", "MethodFunc": "", "All": "",
	"Get": "GET", "GET": "GET", "Head": "HEAD", "HEAD": "HEAD", "Post": "POST", "POST": "POST",
	"Put": "PUT", "PUT": "PUT", "Patch": "PATCH", "PATCH": "PATCH", "Delete": "DELETE", "DELETE": "DELETE",
	"Options": "OPTIONS", "OPTIONS": "OPTIONS", "Connect": "CONNECT", "CONNECT": "CONNECT", "Trace": "TRACE", "TRACE": "TRACE",
}

// notRoutes are packages whose functions take a path and a function but
// register nothing (filepath.Walk, fs.WalkDir).
var notRoutes = []string{"path/filepath", "io/fs", "os", "testing", "flag"}

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

const (
	maxGuards     = 4
	maxGuardText  = 60
	maxWrapDepth  = 4
	cobraPkg      = "github.com/spf13/cobra"
	guardSeparate = "; "
)

// routes records the entry points registered inside node (a function body
// or a package-level declaration) of file rel.
func (x *extractor) routes(p *packages.Package, node ast.Node, rel string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			x.routeCall(p, n, rel)
		case *ast.CompositeLit:
			x.cobraCommand(p, n, rel)
		}
		return true
	})
}

func (x *extractor) routeCall(p *packages.Package, call *ast.CallExpr, rel string) {
	fn := calleeFunc(p.TypesInfo, call.Fun)
	if fn == nil || fn.Pkg() == nil || len(call.Args) < 2 {
		return
	}
	pkg := fn.Pkg().Path()
	if fn.Name() == "AddTool" {
		x.mcpTool(p, call, rel)
		return
	}
	verb, known := routeMethods[fn.Name()]
	if slices.Contains(notRoutes, pkg) || !known && !x.inRepo(strings.TrimSuffix(pkg, "_test")) {
		return
	}
	pat := -1
	var m []string
	for i, a := range call.Args {
		if s, ok := constString(p.TypesInfo, a); ok {
			if m = routePatternRe.FindStringSubmatch(s); m != nil {
				pat = i
				break
			}
		}
	}
	if pat < 0 {
		return
	}
	handler, wrappers, at := "", []string(nil), -1
	for i := len(call.Args) - 1; i > pat; i-- {
		var found bool
		if handler, wrappers, found = x.handlerOf(p, call.Args[i], 0); found {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	var guards []string
	for i, a := range call.Args {
		if i != pat && i != at {
			guards = append(guards, exprText(a))
		}
	}
	method := m[1]
	if method == "" {
		method = verb
	}
	name := strings.TrimSpace(method + " " + m[2] + m[3])
	x.addRoute(p, model.RouteHTTP, name, call, handler, append(guards, wrappers...), rel)
}

// mcpTool records AddTool(server, tool, handler): the tool's name is the
// Name field of a Tool literal, or the first constant string of the call
// that builds it (mcp.NewTool("name", …), a local helper).
func (x *extractor) mcpTool(p *packages.Package, call *ast.CallExpr, rel string) {
	handler, wrappers, found := x.handlerOf(p, call.Args[len(call.Args)-1], 0)
	if !found {
		return
	}
	name := ""
	for _, a := range call.Args[:len(call.Args)-1] {
		if name = toolName(p.TypesInfo, a); name != "" {
			break
		}
	}
	if !toolNameRe.MatchString(name) {
		return
	}
	x.addRoute(p, model.RouteMCP, name, call, handler, wrappers, rel)
}

func toolName(info *types.Info, e ast.Expr) string {
	switch e := ast.Unparen(e).(type) {
	case *ast.UnaryExpr:
		return toolName(info, e.X)
	case *ast.CompositeLit:
		for _, el := range e.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Name" {
					s, _ := constString(info, kv.Value)
					return s
				}
			}
		}
	case *ast.CallExpr:
		for _, a := range e.Args {
			if s, ok := constString(info, a); ok {
				return s
			}
		}
	}
	return ""
}

// cobraCommand records a cobra.Command literal that runs something.
func (x *extractor) cobraCommand(p *packages.Package, lit *ast.CompositeLit, rel string) {
	tv, ok := p.TypesInfo.Types[lit]
	if !ok {
		return
	}
	named, ok := tv.Type.(*types.Named)
	if !ok || named.Obj().Name() != "Command" || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != cobraPkg {
		return
	}
	var use, handler string
	var wrappers []string
	found := false
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch k.Name {
		case "Use":
			use, _ = constString(p.TypesInfo, kv.Value)
		case "Run", "RunE":
			handler, wrappers, found = x.handlerOf(p, kv.Value, 0)
		}
	}
	name, _, _ := strings.Cut(strings.TrimSpace(use), " ")
	if name == "" || !found {
		return
	}
	x.addRoute(p, model.RouteCLI, name, lit, handler, wrappers, rel)
}

// handlerOf finds the function a handler expression runs: a function or
// method value of the repository, the ServeHTTP of a handler value, or the
// innermost handler of wrapper calls (whose names it returns). An inline
// function literal or a handler held in a variable is found without an ID:
// the route is real, but no function of the graph is its handler, and
// naming the enclosing one would credit the route with everything that
// function reaches.
func (x *extractor) handlerOf(p *packages.Package, e ast.Expr, depth int) (id string, wrappers []string, found bool) {
	info := p.TypesInfo
	switch e := ast.Unparen(e).(type) {
	case *ast.FuncLit:
		return "", nil, true
	case *ast.Ident, *ast.SelectorExpr:
		id, _ := e.(*ast.Ident)
		if sel, ok := e.(*ast.SelectorExpr); ok {
			id = sel.Sel
		}
		if fn, ok := info.Uses[id].(*types.Func); ok {
			if fn.Pkg() != nil && x.inRepo(strings.TrimSuffix(fn.Pkg().Path(), "_test")) {
				return x.funcID(fn.Origin()), nil, true
			}
			return "", nil, false
		}
		if tv, ok := info.Types[e]; ok && tv.Type != nil {
			obj, _, _ := types.LookupFieldOrMethod(tv.Type, true, nil, "ServeHTTP")
			if m, ok := obj.(*types.Func); ok && m.Pkg() != nil && x.inRepo(m.Pkg().Path()) {
				return x.funcID(m.Origin()), nil, true
			}
		}
	case *ast.CallExpr:
		if depth >= maxWrapDepth {
			return "", nil, false
		}
		wrapper := ""
		if tv, ok := info.Types[e.Fun]; !ok || !tv.IsType() { // a conversion (http.HandlerFunc(h)) guards nothing
			if fn := calleeFunc(info, e.Fun); fn != nil {
				wrapper = fn.Name()
			}
		}
		for i := len(e.Args) - 1; i >= 0; i-- {
			if h, inner, ok := x.handlerOf(p, e.Args[i], depth+1); ok {
				if wrapper != "" {
					inner = append([]string{wrapper}, inner...)
				}
				return h, inner, true
			}
		}
		// A wrapper around a handler held in a variable.
		for _, a := range e.Args {
			if tv, ok := info.Types[a]; ok && tv.Type != nil && isHandlerType(tv.Type) {
				if wrapper == "" {
					return "", nil, true
				}
				return "", []string{wrapper}, true
			}
		}
		// A factory of the repository that returns the handler
		// (s.handleSSOStart(kind)): the closure it returns lives in it.
		if fn := calleeFunc(info, e.Fun); fn != nil && fn.Pkg() != nil && x.inRepo(strings.TrimSuffix(fn.Pkg().Path(), "_test")) && returnsHandler(fn) {
			return x.funcID(fn.Origin()), nil, true
		}
	}
	return "", nil, false
}

// returnsHandler reports a function whose only result is a handler.
func returnsHandler(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Results().Len() != 1 {
		return false
	}
	return isHandlerType(sig.Results().At(0).Type())
}

// isHandlerType reports a function type or a type with ServeHTTP.
func isHandlerType(t types.Type) bool {
	if _, ok := t.Underlying().(*types.Signature); ok {
		return true
	}
	obj, _, _ := types.LookupFieldOrMethod(t, true, nil, "ServeHTTP")
	_, ok := obj.(*types.Func)
	return ok
}

func (x *extractor) addRoute(p *packages.Package, kind, name string, at ast.Node, handler string, guards []string, rel string) {
	// Two binaries may register the same route (GET /healthz): the
	// registering package keeps them apart.
	pkg := strings.TrimSuffix(p.PkgPath, "_test")
	local := kind + " " + name + " (" + pkg + ")"
	id := x.id(model.KindRoute, local)
	pos := p.Fset.Position(at.Pos())
	guards = guards[:min(len(guards), maxGuards)]
	x.g.AddNode(model.Node{
		ID: id, Kind: model.KindRoute, Repo: x.opts.Repo, Name: name, QualifiedName: local,
		Package: pkg, File: rel, StartLine: pos.Line, EndLine: pos.Line,
		Signature: kind, Summary: redact.Summary(strings.Join(guards, guardSeparate), summaryLen),
	})
	if handler != "" {
		x.g.AddEdge(model.Edge{From: id, To: handler, Kind: model.EdgeHandledBy, Evidence: model.EvidenceTyped})
	}
}

func constString(info *types.Info, e ast.Expr) (string, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// exprText is a short rendering of a registration argument, for guards.
func exprText(e ast.Expr) string {
	s := types.ExprString(e)
	if len(s) > maxGuardText {
		s = s[:maxGuardText] + "…"
	}
	return s
}
