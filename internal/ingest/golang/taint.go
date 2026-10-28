package golang

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Taint tracking inside one function (security roadmap item 3, first
// step): request input is followed through assignments, string building,
// URL parsing, body reads and JSON decoding to the sinks the request rules
// check, so `q := r.FormValue("q"); db.Query("… " + q)` is seen like the
// direct use. Calls into other functions do not carry taint yet, except
// the standard library functions that only transform their input. A value
// parsed into a number, a base name or an escaped string is clean.

// requestTypes are the incoming request. A URL, header or cookie is input
// only when it was read from one (r.URL, r.Header): a url.URL parsed from
// configuration is not.
var requestTypes = map[string]bool{"net/http.Request": true, "*net/http.Request": true}

// requestReads are the methods and fields of request types that return
// input the client chose.
var requestReads = map[string]bool{
	"FormValue": true, "PostFormValue": true, "PathValue": true, "Query": true, "Get": true, "Values": true,
	"URL": true, "Path": true, "RawPath": true, "RawQuery": true, "Form": true, "PostForm": true, "MultipartForm": true,
	"Header": true, "Body": true, "Cookie": true, "Cookies": true, "Referer": true, "UserAgent": true, "Host": true,
	"RequestURI": true, "Value": true, "FormFile": true,
}

// frameworkSources are request readers of web frameworks, by full name of
// the function or method (receiver type without the pointer).
var frameworkSources = map[string]bool{
	"github.com/gorilla/mux.Vars":                    true,
	"github.com/go-chi/chi/v5.URLParam":              true,
	"github.com/go-chi/chi.URLParam":                 true,
	"github.com/gin-gonic/gin.Context.Query":         true,
	"github.com/gin-gonic/gin.Context.DefaultQuery":  true,
	"github.com/gin-gonic/gin.Context.Param":         true,
	"github.com/gin-gonic/gin.Context.PostForm":      true,
	"github.com/gin-gonic/gin.Context.GetHeader":     true,
	"github.com/gin-gonic/gin.Context.GetRawData":    true,
	"github.com/labstack/echo/v4.Context.QueryParam": true,
	"github.com/labstack/echo/v4.Context.Param":      true,
	"github.com/labstack/echo/v4.Context.FormValue":  true,
	"github.com/gofiber/fiber/v2.Ctx.Query":          true,
	"github.com/gofiber/fiber/v2.Ctx.Params":         true,
	"github.com/gofiber/fiber/v2.Ctx.FormValue":      true,
	"github.com/gofiber/fiber/v2.Ctx.Body":           true,
}

// binders fill their pointer argument from the request.
var binders = map[string]bool{
	"github.com/gin-gonic/gin.Context.Bind": true, "github.com/gin-gonic/gin.Context.ShouldBind": true,
	"github.com/gin-gonic/gin.Context.BindJSON": true, "github.com/gin-gonic/gin.Context.ShouldBindJSON": true,
	"github.com/gin-gonic/gin.Context.ShouldBindQuery": true, "github.com/labstack/echo/v4.Context.Bind": true,
	"github.com/gofiber/fiber/v2.Ctx.BodyParser": true,
}

// propagators are packages whose functions return what they were given,
// transformed: the result of a tainted input is tainted.
var propagators = map[string]bool{
	"fmt": true, "strings": true, "bytes": true, "path": true, "path/filepath": true, "net/url": true,
	"io": true, "bufio": true, "encoding/json": true, "encoding/base64": true,
}

// propagatingMethods hand back what their receiver holds, outside the
// propagator packages.
var propagatingMethods = map[string]bool{
	"net/http.Header.Get": true, "net/http.Header.Values": true, "net/http.Cookie.String": true,
}

// sanitizers return values an attacker cannot steer into the sinks here:
// numbers, a file's base name, escaped strings.
var sanitizers = map[string]bool{
	"strconv.Atoi": true, "strconv.ParseInt": true, "strconv.ParseUint": true, "strconv.ParseFloat": true, "strconv.ParseBool": true,
	"path/filepath.Base": true, "path.Base": true, "net/url.QueryEscape": true, "net/url.PathEscape": true,
	"html.EscapeString": true, "html/template.HTMLEscapeString": true, "strings.Count": true, "strings.Contains": true,
	"strings.HasPrefix": true, "strings.HasSuffix": true, "strings.Index": true, "strings.EqualFold": true,
}

// methodName is "pkg.Type.Method" for a method (pointer receivers too) and
// the full name for a function.
func methodName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil || fn.Pkg() == nil {
		return fn.FullName()
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return fn.Pkg().Path() + "." + n.Obj().Name() + "." + fn.Name()
	}
	return fn.FullName()
}

// source names e, as written, when it reads request input itself:
// r.FormValue(…), r.URL, r.Header.Get(…), mux.Vars(r), c.Query(…) and the
// like.
func (s *scanner) source(e ast.Expr) string {
	switch v := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		if tv, ok := s.p.info.Types[v.X]; ok && tv.Type != nil && requestTypes[tv.Type.String()] && requestReads[v.Sel.Name] {
			return types.ExprString(v)
		}
	case *ast.CallExpr:
		if sel, ok := ast.Unparen(v.Fun).(*ast.SelectorExpr); ok {
			if d := s.source(sel); d != "" {
				return d
			}
		}
		if fn := calleeFunc(s.p.info, v.Fun); fn != nil && frameworkSources[methodName(fn.Origin())] {
			return types.ExprString(v.Fun)
		}
	}
	return ""
}

// taintOf describes where e's value comes from in the request, or "".
func (s *scanner) taintOf(e ast.Expr) string {
	if e == nil || s.isConst(e) {
		return ""
	}
	if d := s.source(e); d != "" {
		return d
	}
	switch v := ast.Unparen(e).(type) {
	case *ast.Ident:
		if obj := s.p.info.Uses[v]; obj != nil {
			return s.taint[obj]
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			if d := s.taintOf(v.X); d != "" {
				return d
			}
			return s.taintOf(v.Y)
		}
	case *ast.SelectorExpr:
		if _, isPkg := s.p.info.Uses[identOf(v.X)].(*types.PkgName); !isPkg {
			return s.taintOf(v.X)
		}
	case *ast.IndexExpr:
		return s.taintOf(v.X)
	case *ast.SliceExpr:
		return s.taintOf(v.X)
	case *ast.StarExpr:
		return s.taintOf(v.X)
	case *ast.UnaryExpr:
		return s.taintOf(v.X)
	case *ast.CallExpr:
		if tv, ok := s.p.info.Types[v.Fun]; ok && tv.IsType() && len(v.Args) == 1 {
			return s.taintOf(v.Args[0]) // string(b), []byte(s)
		}
		fn := calleeFunc(s.p.info, v.Fun)
		if fn == nil {
			for _, sum := range s.dynamicSummaries(v) {
				if d := s.returnsTaint(v, sum); d != "" {
					return d
				}
			}
			return ""
		}
		if fn.Pkg() == nil {
			return ""
		}
		if sums := s.summariesOf(fn.Origin()); len(sums) > 0 {
			for _, sum := range sums {
				if d := s.returnsTaint(v, sum); d != "" {
					return d
				}
			}
			return ""
		}
		if sanitizers[fn.FullName()] || !propagators[fn.Pkg().Path()] && !propagatingMethods[methodName(fn.Origin())] {
			return ""
		}
		for _, a := range v.Args {
			if d := s.taintOf(a); d != "" {
				return d
			}
		}
		if sel, ok := ast.Unparen(v.Fun).(*ast.SelectorExpr); ok {
			if _, isPkg := s.p.info.Uses[identOf(sel.X)].(*types.PkgName); !isPkg {
				return s.taintOf(sel.X) // u.String(), dec.Decode
			}
		}
	}
	return ""
}

// carriesText reports the result types through which a function hands its
// input back: strings, byte and string slices, URLs. A struct returned by a
// lookup holds stored data, not the key it was found by, and an error that
// names its input goes nowhere a sink reads.
func carriesText(t types.Type) bool {
	if t == nil {
		return false
	}
	if t.String() == "*net/url.URL" || t.String() == "net/url.Values" {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok {
			return b.Kind() == types.Byte || b.Info()&types.IsString != 0
		}
	}
	return false
}

func identOf(e ast.Expr) *ast.Ident {
	id, _ := ast.Unparen(e).(*ast.Ident)
	return id
}

// track records taint from assignments, ranges and decoders. It runs as
// the function is walked, so a value is tainted from the statement that
// assigns it on.
func (s *scanner) track(n ast.Node) {
	set := func(lhs ast.Expr, from string) {
		if id := identOf(lhs); id != nil && id.Name != "_" {
			obj := s.p.info.Defs[id]
			if obj == nil {
				obj = s.p.info.Uses[id]
			}
			if obj == nil {
				return
			}
			if from == "" {
				delete(s.taint, obj) // reassigned from something clean
				return
			}
			s.taint[obj] = from
		}
	}
	switch v := n.(type) {
	case *ast.AssignStmt:
		switch {
		case len(v.Lhs) == len(v.Rhs):
			for i := range v.Lhs {
				d := s.taintOf(v.Rhs[i])
				if v.Tok == token.ADD_ASSIGN && d == "" {
					continue // x += clean keeps what x had
				}
				set(v.Lhs[i], d)
			}
		case len(v.Rhs) == 1:
			// a, err := f(tainted): the values are tainted, not the error.
			d := s.taintOf(v.Rhs[0])
			for _, l := range v.Lhs {
				if tv, ok := s.p.info.Types[l]; ok && tv.Type != nil && tv.Type.String() == "error" {
					continue
				}
				set(l, d)
			}
		}
	case *ast.ValueSpec:
		for i, name := range v.Names {
			if i < len(v.Values) {
				set(name, s.taintOf(v.Values[i]))
			}
		}
	case *ast.ReturnStmt:
		if s.sum != nil {
			for _, r := range v.Results {
				if !carriesText(s.p.info.TypeOf(r)) {
					continue // a record looked up by the input, or an error naming it
				}
				if i := paramOf(s.taintOf(r)); i >= 0 && i < s.sum.params {
					s.sum.returns[i] = true
				}
			}
		}
	case *ast.RangeStmt:
		if d := s.taintOf(v.X); d != "" {
			set(v.Key, d)
			set(v.Value, d)
		}
	case *ast.CallExpr:
		fn := calleeFunc(s.p.info, v.Fun)
		if fn == nil || fn.Pkg() == nil {
			return
		}
		name := methodName(fn.Origin())
		var into ast.Expr
		switch {
		case name == "encoding/json.Unmarshal" || name == "encoding/xml.Unmarshal":
			if len(v.Args) == 2 && s.taintOf(v.Args[0]) != "" {
				into = v.Args[1]
			}
		case name == "encoding/json.Decoder.Decode" || name == "encoding/xml.Decoder.Decode":
			if sel, ok := ast.Unparen(v.Fun).(*ast.SelectorExpr); ok && len(v.Args) == 1 && s.taintOf(sel.X) != "" {
				into = v.Args[0]
			}
		case binders[name]:
			if len(v.Args) == 1 {
				into = v.Args[0]
			}
		}
		if into != nil {
			if u, ok := ast.Unparen(into).(*ast.UnaryExpr); ok && u.Op == token.AND {
				into = u.X
			}
			set(into, "the decoded request body")
		}
	}
}

// withTaint names where a sink's input came from, for a finding's detail.
func withTaint(detail, from string) string {
	if from == "" {
		return detail
	}
	return detail + " (via " + from + ")"
}
