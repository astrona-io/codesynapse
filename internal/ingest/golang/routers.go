package golang

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/astrona-io/codesynapse/internal/ingest/lang"
)

// Router middleware: guards that apply to every route of a router rather
// than to one registration, seen inside the function that builds it.
//
//	r.Use(auth)                              chi, gin, echo, gorilla/mux, fiber
//	api := r.Group("/api", auth)             gin, echo (prefix and middleware)
//	r.Route("/api", func(r chi.Router) {…})  chi (and r.Group(func(r chi.Router) {…}))
//	r.With(auth).Get("/x", h)                chi
//	s := r.PathPrefix("/api").Subrouter()    gorilla/mux
//	http.ListenAndServe(addr, auth(mux))     a handler wrapped around the whole mux
//
// Use counts for the routes registered after it, as chi and gin apply it; a
// wrapper around the mux counts for all of them. Only names that look like
// access checks are kept (auth yes, Logger no), as for the other languages.
// A router built in one function and filled in another is not followed.

type routerScope struct {
	parent *routerScope
	// since is where this router was derived from its parent: the parent's
	// middleware from before then applies to it.
	since  token.Pos
	prefix string
	uses   []guardAt
	// wraps are handlers wrapped around the whole router.
	wraps []string
}

type guardAt struct {
	pos  token.Pos
	name string
}

// guardsAt is every guard in force for a route registered on s at pos.
func (s *routerScope) guardsAt(pos token.Pos) []string {
	if s == nil {
		return nil
	}
	out := s.parent.guardsAt(s.since)
	for _, g := range s.uses {
		if g.pos < pos {
			out = append(out, g.name)
		}
	}
	return append(out, s.wraps...)
}

func (s *routerScope) fullPrefix() string {
	if s == nil {
		return ""
	}
	return joinPath(s.parent.fullPrefix(), s.prefix)
}

type routerScopes map[types.Object]*routerScope

// routerScopes collects the routers built inside node and their middleware.
func (x *extractor) routerScopes(p *packages.Package, node ast.Node) routerScopes {
	info := p.TypesInfo
	rs := routerScopes{}
	of := func(e ast.Expr) *routerScope {
		obj := exprObject(info, e)
		if obj == nil || !isRouterType(info.TypeOf(e)) {
			return nil
		}
		if rs[obj] == nil {
			rs[obj] = &routerScope{}
		}
		return rs[obj]
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			if obj := exprObject(info, n.Lhs[0]); obj != nil && isRouterType(info.TypeOf(n.Rhs[0])) {
				rs[obj] = x.derive(p, rs, n.Rhs[0])
				if rs[obj] == nil { // a new router (chi.NewRouter(), http.NewServeMux())
					rs[obj] = &routerScope{}
				}
			}
		case *ast.CallExpr:
			sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "Use" {
				if s := of(sel.X); s != nil {
					for _, a := range n.Args {
						s.uses = append(s.uses, guardAt{pos: n.Pos(), name: exprText(a)})
					}
				}
				return true
			}
			// r.Route("/api", func(r chi.Router) {…}), r.Group(func(r chi.Router) {…})
			if ok && len(n.Args) > 0 {
				if lit, isLit := n.Args[len(n.Args)-1].(*ast.FuncLit); isLit && len(lit.Type.Params.List) == 1 && len(lit.Type.Params.List[0].Names) == 1 {
					if parent := of(sel.X); parent != nil {
						child := &routerScope{parent: parent, since: n.Pos()}
						if s, ok := constString(info, n.Args[0]); ok && strings.HasPrefix(s, "/") {
							child.prefix = s
						}
						if obj := info.Defs[lit.Type.Params.List[0].Names[0]]; obj != nil && isRouterType(obj.Type()) {
							rs[obj] = child
						}
					}
					return true
				}
			}
			// auth(mux): a handler wrapped around the whole router.
			if t := info.TypeOf(n); t != nil && isHandlerType(t) {
				for _, a := range n.Args {
					if s := of(a); s != nil {
						s.wraps = append(s.wraps, exprText(n.Fun))
					}
				}
			}
		}
		return true
	})
	return rs
}

// derive is the router an expression makes from another one (r.Group("/api",
// auth), r.With(auth), r.PathPrefix("/api").Subrouter()), or nil.
func (x *extractor) derive(p *packages.Package, rs routerScopes, e ast.Expr) *routerScope {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return nil
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	var parent *routerScope
	if _, ok := ast.Unparen(sel.X).(*ast.CallExpr); ok {
		parent = x.derive(p, rs, sel.X)
	} else if obj := exprObject(p.TypesInfo, sel.X); obj != nil {
		parent = rs[obj]
		if parent == nil && isRouterType(p.TypesInfo.TypeOf(sel.X)) {
			parent = &routerScope{}
			rs[obj] = parent
		}
	}
	if parent == nil {
		return nil
	}
	switch sel.Sel.Name {
	case "Group", "With", "PathPrefix", "Subrouter", "Route", "Prefix":
	default:
		return nil
	}
	s := &routerScope{parent: parent, since: call.Pos()}
	for i, a := range call.Args {
		if str, ok := constString(p.TypesInfo, a); ok && i == 0 && strings.HasPrefix(str, "/") {
			s.prefix = str
			continue
		}
		if _, isLit := a.(*ast.FuncLit); isLit {
			continue
		}
		// Middleware given when deriving applies to everything registered
		// on the result, which comes after (r.With(auth).Get(…) starts at
		// the same position).
		s.uses = append(s.uses, guardAt{pos: token.NoPos, name: exprText(a)})
	}
	return s
}

// routeContext is the prefix and the router guards for a registration call
// on recv at pos.
func (x *extractor) routeContext(p *packages.Package, rs routerScopes, recv ast.Expr, pos token.Pos) (string, []string) {
	var s *routerScope
	if _, ok := ast.Unparen(recv).(*ast.CallExpr); ok {
		s = x.derive(p, rs, recv)
	} else if obj := exprObject(p.TypesInfo, recv); obj != nil {
		s = rs[obj]
	}
	if s == nil {
		return "", nil
	}
	return s.fullPrefix(), lang.AccessGuards(s.guardsAt(pos))
}

// exprObject is the variable or field an expression names.
func exprObject(info *types.Info, e ast.Expr) types.Object {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident:
		return info.ObjectOf(e)
	case *ast.SelectorExpr:
		if v, ok := info.ObjectOf(e.Sel).(*types.Var); ok {
			return v
		}
	}
	return nil
}

// isRouterType reports a type that registers routes and takes middleware.
func isRouterType(t types.Type) bool {
	if t == nil {
		return false
	}
	has := func(name string) bool {
		obj, _, _ := types.LookupFieldOrMethod(t, true, nil, name)
		_, ok := obj.(*types.Func)
		return ok
	}
	return (has("Use") || has("Handle")) && (has("Handle") || has("HandleFunc") || has("Get") || has("GET"))
}

func joinPath(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}
