package golang

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/rules"
)

// RuleIDs are the rules this detector reports. Their titles, severities,
// CWEs and fix guidance live in rules/code/<id>.yaml.
var RuleIDs = []string{
	"exec-shell",
	"exec-variable",
	"sql-concat",
	"cypher-concat",
	"tls-insecure",
	"tls-old-version",
	"weak-hash",
	"weak-cipher",
	"insecure-rand",
	"hardcoded-secret",
	"http-no-timeout",
	"http-client-default",
	"template-unescaped",
	"file-world-writable",
	"path-from-request",
	"ssrf-from-request",
	"sql-from-request",
	"exec-from-request",
	"unbounded-body",
	"unsafe-pointer",
	"jwt-unverified",
}

var secretName = regexp.MustCompile(`(?i)(passw(or)?d|passwd|secret|token|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credential)`)

// placeholder values that are not real secrets.
var placeholder = regexp.MustCompile(`(?i)^(\$\{.*\}|<.*>|x+|\*+|changeme|change-me.*|example.*|dummy|test|password|secret|token|redacted|todo|none|null|env:.*)$`)

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "/bin/sh": true, "/bin/bash": true, "cmd": true, "cmd.exe": true, "powershell": true, "pwsh": true}

type scanner struct {
	x       *extractor
	p       *pkgInfo
	ownerID string
	rel     string
	// readsLimited is set when the function wraps a body with MaxBytesReader.
	readsLimited bool
	usesCrypto   bool
	seen         map[string]bool
	// taint maps variables holding request input to where it came from.
	taint map[types.Object]string
	// quiet scans only to learn a summary (sum); sums are the summaries of
	// the repository's functions known so far (interprocedural.go).
	quiet bool
	sum   *summary
	sums  map[*types.Func]*summary
	lits  map[*ast.FuncLit]*summary // closures reached through function values
	name  string                    // the function scanned, for findings reached through calls
}

// pkgInfo is the slice of packages.Package the scanner needs.
type pkgInfo struct {
	info *types.Info
	fset *token.FileSet
}

func (x *extractor) scan(info *types.Info, fset *token.FileSet, node ast.Node, ownerID, rel string) {
	x.newScanner(info, fset, ownerID, rel).walk(node)
}

func (x *extractor) newScanner(info *types.Info, fset *token.FileSet, ownerID, rel string) *scanner {
	return &scanner{x: x, p: &pkgInfo{info: info, fset: fset}, ownerID: ownerID, rel: rel, seen: map[string]bool{}, taint: map[types.Object]string{}}
}

// walk runs the rules over node, a function body or declaration.
func (s *scanner) walk(node ast.Node) {
	info := s.p.info
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if fn := calleeFunc(info, call.Fun); fn != nil && fn.Pkg() != nil {
				switch {
				case fn.FullName() == "net/http.MaxBytesReader":
					s.readsLimited = true
				case strings.HasPrefix(fn.Pkg().Path(), "crypto/"):
					s.usesCrypto = true
				}
			}
		}
		return true
	})
	ast.Inspect(node, func(n ast.Node) bool {
		s.track(n)
		switch n := n.(type) {
		case *ast.CallExpr:
			s.call(n)
		case *ast.CompositeLit:
			s.composite(n)
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if i < len(n.Rhs) {
					s.secretAssign(lhs, n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if i < len(n.Values) {
					s.secretAssign(name, n.Values[i])
				}
			}
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok {
				if pn, ok := info.Uses[id].(*types.PkgName); ok && pn.Imported().Path() == "unsafe" {
					s.report("unsafe-pointer", n.Pos(), "uses unsafe."+n.Sel.Name)
				}
			}
		}
		return true
	})
}

func (s *scanner) report(ruleID string, pos token.Pos, detail string) {
	if s.quiet {
		return
	}
	line := s.p.fset.Position(pos).Line
	key := fmt.Sprintf("%s:%d:%s", s.rel, line, ruleID)
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.x.addFinding(s.rel, line, s.ownerID, ruleID, detail)
}

// addFinding adds a finding owned by ownerID. The ID is the place and the
// rule, so the same finding reached twice is one node.
func (x *extractor) addFinding(rel string, line int, ownerID, ruleID, detail string) {
	rule := rules.MustGet(ruleID)
	id := x.id(model.KindFinding, fmt.Sprintf("%s:%d:%s", rel, line, ruleID))
	if _, ok := x.g.Node(id); ok {
		return
	}
	x.g.AddNode(model.Node{
		ID: id, Kind: model.KindFinding, Repo: x.opts.Repo, Name: rule.Title, Rule: rule.ID,
		Severity: rule.Severity, CWE: rule.CWE, File: rel, StartLine: line, EndLine: line,
		Summary: redact.Summary(detail+". "+rule.Fix, 300),
	})
	x.g.AddEdge(model.Edge{From: ownerID, To: id, Kind: model.EdgeHasFinding})
}

func (s *scanner) isConst(e ast.Expr) bool {
	tv, ok := s.p.info.Types[e]
	return ok && tv.Value != nil
}

func (s *scanner) constString(e ast.Expr) (string, bool) {
	tv, ok := s.p.info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// builtDynamically reports whether e is a non-constant string built with
// fmt.Sprintf/Sprint or concatenation, the usual shape of injection bugs.
func (s *scanner) builtDynamically(e ast.Expr) bool {
	if s.isConst(e) {
		return false
	}
	switch v := ast.Unparen(e).(type) {
	case *ast.BinaryExpr:
		return v.Op == token.ADD
	case *ast.CallExpr:
		if fn := calleeFunc(s.p.info, v.Fun); fn != nil {
			switch fn.FullName() {
			case "fmt.Sprintf", "fmt.Sprint", "strings.Join", "strings.ReplaceAll", "strings.Replace":
				return true
			}
		}
	case *ast.Ident:
		// A variable: flag only when it was assigned from a dynamic build in
		// the same function is beyond this scanner; treat as unknown.
		return false
	}
	return false
}

func (s *scanner) call(call *ast.CallExpr) {
	fn := calleeFunc(s.p.info, call.Fun)
	if fn == nil {
		for _, sum := range s.dynamicSummaries(call) {
			s.through(call, types.ExprString(call.Fun), sum)
		}
		s.conversion(call)
		return
	}
	if fn.Pkg() == nil {
		return
	}
	fn = fn.Origin()
	full, pkg := fn.FullName(), fn.Pkg().Path()
	args := call.Args
	for _, sum := range s.summariesOf(fn) {
		s.through(call, fn.Name(), sum)
	}

	switch full {
	case "os/exec.Command", "os/exec.CommandContext":
		off := 0
		if full == "os/exec.CommandContext" {
			off = 1
		}
		if len(args) <= off {
			return
		}
		for i, a := range args[off:] {
			if from := s.taintOf(a); from != "" {
				what := "the program name"
				if i > 0 {
					what = fmt.Sprintf("argument %d", i)
				}
				s.sink("exec-from-request", call.Pos(), what+" comes from the incoming request", from)
				return
			}
		}
		prog, isConst := s.constString(args[off])
		if !isConst {
			s.report("exec-variable", call.Pos(), "program name is not a constant")
			return
		}
		if shells[prog] {
			for i := off + 1; i < len(args)-1; i++ {
				if flag, _ := s.constString(args[i]); (flag == "-c" || flag == "/c" || flag == "-Command") && !s.isConst(args[i+1]) {
					s.report("exec-shell", call.Pos(), prog+" "+flag+" with a non-constant script")
				}
			}
		}
	case "net/http.ListenAndServe", "net/http.ListenAndServeTLS", "net/http.Serve", "net/http.ServeTLS":
		s.report("http-no-timeout", call.Pos(), full+" has no timeouts")
	case "net/http.Get", "net/http.Post", "net/http.PostForm", "net/http.Head":
		s.report("http-client-default", call.Pos(), full)
		if len(args) > 0 {
			if from := s.taintOf(args[0]); from != "" {
				s.sink("ssrf-from-request", call.Pos(), "URL comes from the incoming request", from)
			}
		}
	case "(*net/http.Client).Get", "(*net/http.Client).Post", "(*net/http.Client).Head", "(*net/http.Client).PostForm":
		if len(args) > 0 {
			if from := s.taintOf(args[0]); from != "" {
				s.sink("ssrf-from-request", call.Pos(), "URL comes from the incoming request", from)
			}
		}
	case "net/http.NewRequest", "net/http.NewRequestWithContext":
		if i := len(args) - 2; i >= 0 {
			if from := s.taintOf(args[i]); from != "" {
				s.sink("ssrf-from-request", call.Pos(), "URL comes from the incoming request", from)
			}
		}
	case "os.WriteFile", "os.Chmod", "os.MkdirAll", "os.Mkdir", "os.OpenFile":
		if len(args) > 0 {
			if mode, ok := s.constInt(args[len(args)-1]); ok && mode&0o002 != 0 {
				s.report("file-world-writable", call.Pos(), fmt.Sprintf("%s with mode %#o", full, mode))
			}
			if from := s.taintOf(args[0]); from != "" {
				s.sink("path-from-request", call.Pos(), full+" path comes from the request", from)
			}
		}
	case "os.Open", "os.ReadFile", "os.Create", "os.Remove", "os.RemoveAll", "os.ReadDir", "os.Stat", "net/http.ServeFile":
		// The path is the last argument of ServeFile, the first elsewhere.
		a := args[0:min(1, len(args))]
		if full == "net/http.ServeFile" && len(args) == 3 {
			a = args[2:]
		}
		for _, x := range a {
			if from := s.taintOf(x); from != "" {
				s.sink("path-from-request", call.Pos(), full+" path comes from the request", from)
				break
			}
		}
	case "io.ReadAll":
		if len(args) == 1 && !s.readsLimited {
			if sel, ok := ast.Unparen(args[0]).(*ast.SelectorExpr); ok && sel.Sel.Name == "Body" {
				if tv, ok := s.p.info.Types[sel.X]; ok && strings.HasSuffix(tv.Type.String(), "net/http.Request") {
					s.report("unbounded-body", call.Pos(), "io.ReadAll(r.Body)")
				}
			}
		}
	}

	switch {
	case pkg == "crypto/md5" || pkg == "crypto/sha1":
		s.report("weak-hash", call.Pos(), full)
	case pkg == "crypto/des" || pkg == "crypto/rc4":
		s.report("weak-cipher", call.Pos(), full)
	case (pkg == "math/rand" || pkg == "math/rand/v2") && s.usesCrypto:
		s.report("insecure-rand", call.Pos(), full+" in a function that also uses crypto")
	case strings.HasPrefix(pkg, "github.com/golang-jwt/jwt") && (fn.Name() == "ParseUnverified"):
		s.report("jwt-unverified", call.Pos(), full)
	case pkg == "database/sql" || pkg == "gorm.io/gorm" || strings.HasPrefix(pkg, "github.com/jackc/pgx") || pkg == "github.com/jmoiron/sqlx":
		switch fn.Name() {
		case "Query", "QueryContext", "QueryRow", "QueryRowContext", "Exec", "ExecContext", "Prepare", "PrepareContext", "Raw", "Select", "Get":
			if q := queryArg(fn, args); q != nil {
				if from := s.taintOf(q); from != "" {
					s.sink("sql-from-request", call.Pos(), full+" runs SQL text taken from the request", from)
				} else if s.builtDynamically(q) {
					s.report("sql-concat", call.Pos(), full+" with a dynamically built query")
				}
			}
		}
	case strings.HasPrefix(pkg, "github.com/neo4j/neo4j-go-driver"):
		switch fn.Name() {
		case "ExecuteQuery", "Run":
			if q := queryArg(fn, args); q != nil && s.builtDynamically(q) {
				s.report("cypher-concat", call.Pos(), full+" with a dynamically built query")
			}
		}
	}
}

// queryArg returns the first string-typed argument after any context or
// driver arguments, which is where every supported API takes its query.
func queryArg(fn *types.Func, args []ast.Expr) ast.Expr {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil
	}
	for i := 0; i < sig.Params().Len() && i < len(args); i++ {
		if b, ok := sig.Params().At(i).Type().Underlying().(*types.Basic); ok && b.Kind() == types.String {
			return args[i]
		}
	}
	return nil
}

func (s *scanner) constInt(e ast.Expr) (int64, bool) {
	tv, ok := s.p.info.Types[e]
	if !ok || tv.Value == nil {
		return 0, false
	}
	v, ok := constant.Int64Val(constant.ToInt(tv.Value))
	return v, ok
}

// conversion flags html/template safe-type conversions of non-constants.
func (s *scanner) conversion(call *ast.CallExpr) {
	tv, ok := s.p.info.Types[call.Fun]
	if !ok || !tv.IsType() || len(call.Args) != 1 {
		return
	}
	switch tv.Type.String() {
	case "html/template.HTML", "html/template.JS", "html/template.URL", "html/template.HTMLAttr", "html/template.CSS", "html/template.JSStr", "html/template.Srcset":
		if !s.isConst(call.Args[0]) {
			s.report("template-unescaped", call.Pos(), tv.Type.String()+"(non-constant)")
		}
	}
}

func (s *scanner) composite(lit *ast.CompositeLit) {
	tv, ok := s.p.info.Types[lit]
	if !ok {
		return
	}
	switch strings.TrimPrefix(tv.Type.String(), "*") {
	case "crypto/tls.Config":
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, _ := kv.Key.(*ast.Ident)
			if key == nil {
				continue
			}
			switch key.Name {
			case "InsecureSkipVerify":
				if v, ok := s.p.info.Types[kv.Value]; !ok || v.Value == nil || constant.BoolVal(v.Value) {
					s.report("tls-insecure", kv.Pos(), "InsecureSkipVerify is not false")
				}
			case "MinVersion":
				if v, ok := s.constInt(kv.Value); ok && v < 0x0303 {
					s.report("tls-old-version", kv.Pos(), fmt.Sprintf("MinVersion %#x", v))
				}
			}
		}
	case "net/http.Server":
		has := map[string]bool{}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					has[key.Name] = true
				}
			}
		}
		if !has["ReadHeaderTimeout"] && !has["ReadTimeout"] {
			s.report("http-no-timeout", lit.Pos(), "http.Server without ReadHeaderTimeout or ReadTimeout")
		}
	}
	// Secrets in struct literals: Config{Password: "hunter2..."}.
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			s.secretAssign(kv.Key, kv.Value)
		}
	}
}

// secretAssign flags string constants assigned to secret-looking names.
// The value itself is never stored.
func (s *scanner) secretAssign(lhs, rhs ast.Expr) {
	name := exprName(lhs)
	if name == "" || !secretName.MatchString(name) || strings.HasPrefix(strings.ToLower(name), "env") {
		return
	}
	val, ok := s.constString(rhs)
	if !ok || len(val) < 8 || placeholder.MatchString(val) || strings.ContainsAny(val, " /") || isEnvName(val) {
		return
	}
	s.report("hardcoded-secret", rhs.Pos(), fmt.Sprintf("%s is assigned a %d-character constant", name, len(val)))
}

func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.BasicLit:
		return strings.Trim(v.Value, `"`+"`")
	}
	return ""
}

// isEnvName recognises values like "CODESYNAPSE_API_TOKEN": names of secrets,
// not secrets.
func isEnvName(v string) bool {
	for _, c := range v {
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
