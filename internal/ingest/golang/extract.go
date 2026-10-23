// Package golang extracts a type-checked code graph from Go modules using
// golang.org/x/tools/go/packages. Calls are resolved through go/types, so the
// call graph is precise for static calls and interface dispatch within the
// repository.
package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/logx"
	"golang.org/x/tools/go/packages"

	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// summaryLen bounds stored doc summaries (runes).
const summaryLen = 200

// Options configures extraction.
type Options struct {
	// Repo is the repository name used to scope node IDs.
	Repo string
	// Root is the absolute repository root.
	Root string
	// Patterns are go/packages patterns; default "./...".
	Patterns []string
	// AllowNetwork permits module downloads. Off by default so indexing an
	// untrusted repository cannot reach the network through `go list`.
	AllowNetwork bool
}

// Result is the output of Extract.
type Result struct {
	Graph *model.Graph
	// ExternalCalls maps the full name of every called out-of-repo function
	// (types.Func.FullName, e.g. "(*net/http.Client).Do") to caller node IDs.
	// It is not persisted; vulnerability analysis uses it for reachability.
	ExternalCalls map[string][]string
	// ExternalImports maps out-of-repo import paths to importing package IDs.
	ExternalImports map[string][]string
}

// Extract loads every package under opts.Root and returns its code graph.
//
// Loading runs `go list`, which reads go.mod. To keep that safe on untrusted
// repositories the toolchain is pinned to the local one (no toolchain
// downloads), cgo is disabled (cgo directives can invoke pkg-config), and the
// module proxy is disabled unless AllowNetwork is set.
func Extract(ctx context.Context, opts Options) (*Result, error) {
	if len(opts.Patterns) == 0 {
		opts.Patterns = []string{"./..."}
	}
	// Resolve symlinks so relative paths computed against go list output
	// (which reports resolved paths) are stable.
	root, err := filepath.EvalSymlinks(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	opts.Root = root
	env := append(os.Environ(),
		"CGO_ENABLED=0",
		"GOTOOLCHAIN=local",
		"GOFLAGS=", // default -mod: vendor when present, else readonly (sources may be mounted read-only)
		"GOWORK=off",
	)
	if !opts.AllowNetwork {
		env = append(env, "GOPROXY=off", "GONOSUMDB=*")
	}
	env, err = execution.Environment(ctx, env)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Context: ctx,
		Dir:     opts.Root,
		Env:     env,
		Tests:   true,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule,
	}
	pkgs, err := loadPackages(ctx, cfg, opts.Patterns, repoconfig.FromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	logger := logx.FromContext(ctx)
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if len(p.Errors) > 0 {
			for _, filename := range p.GoFiles {
				if rel, err := filepath.Rel(root, filename); err == nil && repoconfig.FromContext(ctx).PathAllowed(filepath.ToSlash(rel)) {
					ingestreport.Observe(ctx, filepath.ToSlash(rel), ingestreport.Go, ingestreport.StageFailed, "package_load_failed")
				}
			}
		}
		for _, e := range p.Errors {
			logger.Debug("package load error", "package", p.ID, "error", e.Msg)
		}
	})

	x := &extractor{
		opts:    opts,
		policy:  repoconfig.FromContext(ctx),
		g:       model.NewGraph(),
		modules: map[string]bool{},
		srcs:    map[string][]byte{},
		res:     &Result{ExternalCalls: map[string][]string{}, ExternalImports: map[string][]string{}},
	}
	x.g.AddNode(model.Node{ID: x.repoID(), Kind: model.KindRepository, Repo: opts.Repo, Name: opts.Repo})
	x.nested = nestedRepos(opts.Root, x.policy)

	for _, p := range pkgs {
		if p.Module != nil {
			x.modules[p.Module.Path] = true
		}
	}

	// Pass 1: primary packages provide declarations and the type universe
	// used for interface satisfaction. Pass 2 adds test files from test
	// variants, whose objects are distinct but share qualified names.
	var primary []*packages.Package
	for _, p := range pkgs {
		if isTestMain(p) || isTestVariant(p) {
			continue
		}
		primary = append(primary, p)
		x.pkg(p, false)
	}
	for _, p := range pkgs {
		if isTestMain(p) || !isTestVariant(p) {
			continue
		}
		x.pkg(p, true)
	}
	x.implementations(primary)
	x.g.PruneDanglingEdges()
	for _, node := range x.g.Nodes() {
		if node.Kind == model.KindFile {
			ingestreport.Observe(ctx, node.File, ingestreport.Go, ingestreport.StageSucceeded, "")
		}
	}
	x.res.Graph = x.g
	return x.res, nil
}

// isTestMain reports the synthesized "pkg.test" main package.
func isTestMain(p *packages.Package) bool { return strings.HasSuffix(p.PkgPath, ".test") }

// isTestVariant reports "pkg [pkg.test]" and external "pkg_test" packages.
func isTestVariant(p *packages.Package) bool {
	return p.ID != p.PkgPath || strings.HasSuffix(p.PkgPath, "_test")
}

type extractor struct {
	policy  *repoconfig.Policy
	opts    Options
	g       *model.Graph
	modules map[string]bool
	srcs    map[string][]byte
	res     *Result
	// nested holds repo-relative prefixes ("sources/other/") of nested git
	// repositories, which are separate graphs and must not be merged in.
	nested []string
	// generated is set while a generated file is walked: machine output is
	// not worth sharing.
	generated bool
}

// nestedRepos finds directories below root that contain their own .git.
func nestedRepos(root string, policies ...*repoconfig.Policy) []string {
	var policy *repoconfig.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // best-effort
		if err != nil || !d.IsDir() || path == root {
			return nil //nolint:nilerr // unreadable dirs are skipped
		}
		if allowed, policyErr := policy.WalkEntry(root, path, d); !allowed || policyErr != nil {
			return policyErr
		}
		name := d.Name()
		if name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
			if rel, err := filepath.Rel(root, path); err == nil {
				out = append(out, filepath.ToSlash(rel)+"/")
			}
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

func (x *extractor) repoID() string {
	return model.NodeID(x.opts.Repo, model.KindRepository, x.opts.Repo)
}

func (x *extractor) id(kind model.NodeKind, local string) string {
	return model.NodeID(x.opts.Repo, kind, local)
}

// inRepo reports whether an import path belongs to a module in this repository.
func (x *extractor) inRepo(pkgPath string) bool {
	for m := range x.modules {
		if pkgPath == m || strings.HasPrefix(pkgPath, m+"/") {
			return true
		}
	}
	return false
}

// relPath converts an absolute file name to a repo-relative slash path, or
// "" when the file lies outside the repository (build cache, GOROOT).
func (x *extractor) relPath(abs string) string {
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(x.opts.Root, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (x *extractor) inNested(rel string) bool {
	for _, prefix := range x.nested {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	return false
}

// pkgInRepo reports whether any of the package's files belong to this
// repository (and not to a nested one).
func (x *extractor) pkgInRepo(p *packages.Package) bool {
	for _, f := range p.CompiledGoFiles {
		if rel := x.relPath(f); rel != "" && !x.inNested(rel) && x.policy.PathAllowed(rel) {
			return true
		}
	}
	return len(p.CompiledGoFiles) == 0
}

func (x *extractor) pkg(p *packages.Package, testOnly bool) {
	if !x.pkgInRepo(p) {
		return
	}
	pkgPath := strings.TrimSuffix(p.PkgPath, "_test")
	pkgID := x.id(model.KindPackage, pkgPath)
	pkgNode := x.g.AddNode(model.Node{
		ID: pkgID, Kind: model.KindPackage, Repo: x.opts.Repo,
		Name: p.Name, QualifiedName: pkgPath, Package: pkgPath,
	})
	x.g.AddEdge(model.Edge{From: x.repoID(), To: pkgID, Kind: model.EdgeContains})

	if !testOnly {
		for imp := range x.permittedImports(p) {
			if x.inRepo(imp) {
				x.g.AddEdge(model.Edge{From: pkgID, To: x.id(model.KindPackage, imp), Kind: model.EdgeImports})
			} else {
				x.res.ExternalImports[imp] = append(x.res.ExternalImports[imp], pkgID)
			}
		}
	}

	for i, f := range p.Syntax {
		if i >= len(p.CompiledGoFiles) {
			break
		}
		abs := p.CompiledGoFiles[i]
		rel := x.relPath(abs)
		isTestFile := strings.HasSuffix(abs, "_test.go")
		if rel == "" || (testOnly && !isTestFile) || strings.HasPrefix(rel, "vendor/") || x.inNested(rel) || !x.policy.PathAllowed(rel) {
			continue
		}
		if pkgNode.Summary == "" && f.Doc != nil {
			pkgNode.Summary = redact.Summary(redact.FirstSentence(f.Doc.Text()), summaryLen)
		}
		x.file(p, f, abs, rel, pkgPath, isTestFile)
	}
}

// Package imports include ignored files too; use only declarations emitted here.
func (x *extractor) permittedImports(p *packages.Package) map[string]bool {
	imports := map[string]bool{}
	for i, file := range p.Syntax {
		if i >= len(p.CompiledGoFiles) {
			break
		}
		rel := x.relPath(p.CompiledGoFiles[i])
		if rel == "" || x.inNested(rel) || !x.policy.PathAllowed(rel) {
			continue
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err == nil {
				imports[path] = true
			}
		}
	}
	return imports
}

func (x *extractor) source(abs string) []byte {
	if b, ok := x.srcs[abs]; ok {
		return b
	}
	b, err := os.ReadFile(abs) //nolint:gosec // abs comes from go list for files inside Root
	if err != nil {
		b = nil
	}
	x.srcs[abs] = b
	return b
}

func (x *extractor) file(p *packages.Package, f *ast.File, abs, rel, pkgPath string, isTest bool) {
	fset := p.Fset
	x.generated = ast.IsGenerated(f)
	fileID := x.id(model.KindFile, rel)
	tf := fset.File(f.Pos())
	lines, size := 0, 0
	if tf != nil {
		lines, size = tf.LineCount(), tf.Size()
	}
	x.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindFile, Repo: x.opts.Repo, Name: filepath.Base(rel),
		File: rel, Package: pkgPath, IsTest: isTest, LOC: lines, Bytes: size,
	})
	x.g.AddEdge(model.Edge{From: x.id(model.KindPackage, pkgPath), To: fileID, Kind: model.EdgeContains})

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			x.funcDecl(p, d, abs, rel, fileID, pkgPath, isTest)
		case *ast.GenDecl:
			if d.Tok == token.VAR || d.Tok == token.CONST {
				if !isTest {
					x.scan(p.TypesInfo, p.Fset, d, fileID, rel)
				}
				// Package-level initialisers (tables of handlers, var x = f())
				// run at init and keep what they call or reference alive; the
				// File node stands in as the caller.
				x.calls(p, d, fileID)
				continue
			}
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				x.typeSpec(p, d, spec.(*ast.TypeSpec), rel, fileID, pkgPath)
			}
		}
	}
}

func (x *extractor) funcDecl(p *packages.Package, d *ast.FuncDecl, abs, rel, fileID, pkgPath string, isTestFile bool) {
	obj, ok := p.TypesInfo.Defs[d.Name].(*types.Func)
	if !ok {
		return
	}
	fullName := obj.FullName()
	kind := model.KindFunction
	sig := obj.Type().(*types.Signature)
	if sig.Recv() != nil {
		kind = model.KindMethod
	}
	start := p.Fset.Position(d.Pos())
	end := p.Fset.Position(d.End())
	n := model.Node{
		ID: x.id(kind, fullName), Kind: kind, Repo: x.opts.Repo,
		Name: obj.Name(), QualifiedName: fullName, Package: pkgPath, File: rel,
		StartLine: start.Line, EndLine: end.Line, LOC: end.Line - start.Line + 1,
		Signature: types.ObjectString(obj, types.RelativeTo(p.Types)),
		Exported:  obj.Exported(),
		IsTest:    isTestFile && isTestFunc(obj.Name(), sig),
	}
	if d.Doc != nil {
		n.Summary = redact.Summary(redact.FirstSentence(d.Doc.Text()), summaryLen)
	}
	if src := x.source(abs); src != nil && end.Offset <= len(src) && start.Offset < end.Offset {
		sum := sha256.Sum256(src[start.Offset:end.Offset])
		n.BodyHash = hex.EncodeToString(sum[:8])
	}
	if !isTestFile && !x.generated && n.LOC >= minCloneLOC {
		n.CloneHash, n.CloneSig = cloneHash(p.TypesInfo, d)
	}
	if d.Body != nil {
		n.Complexity = cyclomatic(d.Body)
		n.SecurityTags = x.calls(p, d.Body, n.ID)
		if !isTestFile {
			x.scan(p.TypesInfo, p.Fset, d.Body, n.ID, rel)
		}
	}
	x.g.AddNode(n)
	x.g.AddEdge(model.Edge{From: fileID, To: n.ID, Kind: model.EdgeDefines})

	if kind == model.KindMethod {
		if named := receiverNamed(sig.Recv().Type()); named != nil {
			x.g.AddEdge(model.Edge{From: x.typeID(named.Obj()), To: n.ID, Kind: model.EdgeHasMethod})
		}
	}
}

func isTestFunc(name string, sig *types.Signature) bool {
	if sig.Recv() != nil {
		return false
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func receiverNamed(t types.Type) *types.Named {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, _ := t.(*types.Named)
	return named
}

func (x *extractor) typeID(obj *types.TypeName) string {
	pkg := ""
	if obj.Pkg() != nil {
		pkg = strings.TrimSuffix(obj.Pkg().Path(), "_test")
	}
	return x.id(model.KindType, pkg+"."+obj.Name())
}

func (x *extractor) typeSpec(p *packages.Package, gd *ast.GenDecl, ts *ast.TypeSpec, rel, fileID, pkgPath string) {
	obj, ok := p.TypesInfo.Defs[ts.Name].(*types.TypeName)
	if !ok || obj.Parent() != obj.Pkg().Scope() {
		return // skip function-local types
	}
	start := p.Fset.Position(ts.Pos())
	end := p.Fset.Position(ts.End())
	n := model.Node{
		ID: x.typeID(obj), Kind: model.KindType, Repo: x.opts.Repo,
		Name: obj.Name(), QualifiedName: pkgPath + "." + obj.Name(), Package: pkgPath, File: rel,
		StartLine: start.Line, EndLine: end.Line, LOC: end.Line - start.Line + 1,
		Signature: typeKind(obj.Type()), Exported: obj.Exported(),
	}
	doc := ts.Doc
	if doc == nil && len(gd.Specs) == 1 {
		doc = gd.Doc
	}
	if doc != nil {
		n.Summary = redact.Summary(redact.FirstSentence(doc.Text()), summaryLen)
	}
	x.g.AddNode(n)
	x.g.AddEdge(model.Edge{From: fileID, To: n.ID, Kind: model.EdgeDefines})

	iface, ok := obj.Type().Underlying().(*types.Interface)
	if !ok {
		return
	}
	for m := range iface.ExplicitMethods() {
		pos := p.Fset.Position(m.Pos())
		mn := model.Node{
			ID: x.id(model.KindInterfaceMethod, m.FullName()), Kind: model.KindInterfaceMethod,
			Repo: x.opts.Repo, Name: m.Name(), QualifiedName: m.FullName(), Package: pkgPath,
			File: rel, StartLine: pos.Line, EndLine: pos.Line,
			Signature: types.ObjectString(m, types.RelativeTo(p.Types)), Exported: m.Exported(),
		}
		x.g.AddNode(mn)
		x.g.AddEdge(model.Edge{From: n.ID, To: mn.ID, Kind: model.EdgeHasMethod})
	}
}

func typeKind(t types.Type) string {
	switch t.Underlying().(type) {
	case *types.Struct:
		return "struct"
	case *types.Interface:
		return "interface"
	case *types.Signature:
		return "func"
	case *types.Map:
		return "map"
	case *types.Slice:
		return "slice"
	default:
		return "type"
	}
}

// calls records CALLS edges for every call inside body and returns the
// security tags implied by the callees.
func (x *extractor) calls(p *packages.Package, body ast.Node, callerID string) []string {
	var tags []string
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calleeFunc(p.TypesInfo, call.Fun)
		if fn == nil || fn.Pkg() == nil {
			return true
		}
		fn = fn.Origin()
		calleePkg := fn.Pkg().Path()
		tag := tagFor(calleePkg, fn.FullName())
		tags = addTag(tags, tag)

		switch {
		case x.inRepo(strings.TrimSuffix(calleePkg, "_test")):
			kind := model.KindFunction
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
				kind = model.KindMethod
				if types.IsInterface(sig.Recv().Type()) {
					kind = model.KindInterfaceMethod
				}
			}
			x.g.AddEdge(model.Edge{From: callerID, To: x.id(kind, fn.FullName()), Kind: model.EdgeCalls})
		default:
			x.res.ExternalCalls[fn.FullName()] = append(x.res.ExternalCalls[fn.FullName()], callerID)
			if tag == "" {
				break
			}
			// Sensitive external APIs become nodes so "who calls exec.Command"
			// is a single graph hop. Other external calls are dropped to keep
			// the graph (and answers) small.
			extID := x.id(model.KindExternal, fn.FullName())
			x.g.AddNode(model.Node{
				ID: extID, Kind: model.KindExternal, Repo: x.opts.Repo, Name: fn.Name(),
				QualifiedName: fn.FullName(), Package: calleePkg, SecurityTags: []string{tag},
			})
			x.g.AddEdge(model.Edge{From: callerID, To: extID, Kind: model.EdgeCalls})
		}
		return true
	})
	x.references(p, body, callerID)
	// unsafe has no callable functions worth tracking (Pointer is a type), so
	// detect any use of the package directly.
	ast.Inspect(body, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			if pn, ok := p.TypesInfo.Uses[id].(*types.PkgName); ok && pn.Imported().Path() == "unsafe" {
				tags = addTag(tags, TagUnsafe)
			}
		}
		return true
	})
	return tags
}

// references records REFERENCES edges for in-repo functions and methods
// used as values inside node (passed as handlers or callbacks, stored in
// maps, method values), which the call graph alone would miss.
func (x *extractor) references(p *packages.Package, node ast.Node, fromID string) {
	called := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch f := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				called[f] = true
			case *ast.SelectorExpr:
				called[f.Sel] = true
			case *ast.IndexExpr:
				if id, ok := f.X.(*ast.Ident); ok {
					called[id] = true
				}
			}
		}
		return true
	})
	ast.Inspect(node, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || called[id] {
			return true
		}
		fn, ok := p.TypesInfo.Uses[id].(*types.Func)
		if !ok || fn.Pkg() == nil || !x.inRepo(strings.TrimSuffix(fn.Pkg().Path(), "_test")) {
			return true
		}
		fn = fn.Origin()
		kind := model.KindFunction
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			kind = model.KindMethod
			if types.IsInterface(sig.Recv().Type()) {
				kind = model.KindInterfaceMethod
			}
		}
		x.g.AddEdge(model.Edge{From: fromID, To: x.id(kind, fn.FullName()), Kind: model.EdgeReferences})
		return true
	})
}

// calleeFunc resolves the static callee of a call expression, or nil for
// calls through function values, conversions and builtins.
func calleeFunc(info *types.Info, fun ast.Expr) *types.Func {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	case *ast.IndexExpr: // generic instantiation f[T](...)
		return calleeFunc(info, f.X)
	case *ast.IndexListExpr:
		return calleeFunc(info, f.X)
	}
	return nil
}

// cyclomatic computes McCabe complexity: 1 + decision points.
func cyclomatic(body *ast.BlockStmt) int {
	c := 1
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			c++
		case *ast.CaseClause:
			if n.List != nil {
				c++
			}
		case *ast.CommClause:
			if n.Comm != nil {
				c++
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				c++
			}
		}
		return true
	})
	return c
}

// implementations adds IMPLEMENTS edges between in-repo concrete types and
// interfaces, and DISPATCHES_TO edges from each interface method to the
// concrete method that satisfies it.
func (x *extractor) implementations(pkgs []*packages.Package) {
	var concrete, ifaces []*types.TypeName
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			if _, ok := x.g.Node(x.typeID(tn)); !ok {
				continue
			}
			if iface, ok := tn.Type().Underlying().(*types.Interface); ok {
				if iface.NumMethods() > 0 {
					ifaces = append(ifaces, tn)
				}
				continue
			}
			concrete = append(concrete, tn)
		}
	}
	for _, it := range ifaces {
		iface := it.Type().Underlying().(*types.Interface)
		for _, ct := range concrete {
			ptr := types.NewPointer(ct.Type())
			if !types.Implements(ct.Type(), iface) && !types.Implements(ptr, iface) {
				continue
			}
			x.g.AddEdge(model.Edge{From: x.typeID(ct), To: x.typeID(it), Kind: model.EdgeImplements})
			for m := range iface.Methods() {
				obj, _, _ := types.LookupFieldOrMethod(ptr, true, m.Pkg(), m.Name())
				impl, ok := obj.(*types.Func)
				if !ok {
					continue
				}
				x.g.AddEdge(model.Edge{
					From: x.id(model.KindInterfaceMethod, m.FullName()),
					To:   x.id(model.KindMethod, impl.Origin().FullName()),
					Kind: model.EdgeDispatchesTo,
				})
			}
		}
	}
}
