// Package upgrade answers "what does upgrading this Go module touch?"
// (docs/PLAN.md, "Dependency upgrade impact"): where the repository uses the
// module, and, when both versions are in the local module cache, which of
// the exported API it uses changed between them. Nothing is downloaded: the
// module cache is read as go list reads it with GOPROXY=off.
package upgrade

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/tools/go/packages"

	"github.com/astrona-io/codesynapse/internal/ingest/golang"
)

// MaxUses bounds the uses a report lists; counts stay exact.
const MaxUses = 500

// Use is one place the repository refers to the module's API.
type Use struct {
	// Symbol is "import/path.Name" or, for methods and fields,
	// "import/path.Type.Member".
	Symbol string `json:"symbol"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	// In is the function the use is in, when it is in one.
	In string `json:"in,omitempty"`
}

// Change is an exported declaration that differs between the two versions.
type Change struct {
	Symbol string `json:"symbol"`
	Kind   string `json:"kind"` // removed, changed or added
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	Uses   int    `json:"uses"`
}

// Report is the impact of moving Module from From to To.
type Report struct {
	Module string `json:"module"`
	From   string `json:"from"`
	To     string `json:"to,omitempty"`
	// Uses are sorted by symbol and place, at most MaxUses of UseCount.
	Uses     []Use `json:"uses"`
	UseCount int   `json:"use_count"`
	Symbols  int   `json:"symbols"`
	Files    int   `json:"files"`
	// Compared is false when a version is missing from the module cache
	// (Missing names it) or no target version was given.
	Compared bool     `json:"compared"`
	Missing  []string `json:"missing,omitempty"`
	// Breaking are removed or changed declarations the repository uses.
	Breaking []Change `json:"breaking,omitempty"`
	// Changed, Removed and Added count the module's whole exported API.
	Changed int `json:"changed"`
	Removed int `json:"removed"`
	Added   int `json:"added"`
}

// Options select the repository, the module and the target version.
type Options struct {
	Root   string
	Module string
	// To is the version to upgrade to; empty lists the uses only.
	To string
}

// Analyze loads the repository's packages and compares the module's
// current version (from the build list) with To.
func Analyze(ctx context.Context, opts Options) (*Report, error) {
	if err := module.CheckPath(opts.Module); err != nil {
		return nil, err
	}
	if opts.To != "" {
		if err := module.Check(opts.Module, opts.To); err != nil {
			return nil, err
		}
	}
	root, err := filepath.EvalSymlinks(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	pkgs, err := golang.Load(ctx, root, packages.NeedName|packages.NeedFiles|packages.NeedSyntax|
		packages.NeedTypes|packages.NeedTypesInfo|packages.NeedImports|packages.NeedModule)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}
	from := currentVersion(pkgs, opts.Module)
	if from == "" {
		return nil, fmt.Errorf("no package of %s is imported here, or its version is unknown (a replace with a local directory has none)", opts.Module)
	}
	uses := FindUses(pkgs, root, opts.Module)
	r := &Report{Module: opts.Module, From: from, To: opts.To, UseCount: len(uses), Uses: uses[:min(len(uses), MaxUses)], Symbols: countSymbols(uses), Files: countFiles(uses)}
	if opts.To == "" || opts.To == from {
		return r, nil
	}
	cache, err := modCache(ctx)
	if err != nil {
		return nil, err
	}
	apis := make([]map[string]string, 2)
	for i, v := range []string{from, opts.To} {
		dir, err := moduleDir(cache, opts.Module, v)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			r.Missing = append(r.Missing, v)
			continue
		}
		if apis[i], err = API(dir, opts.Module); err != nil {
			return nil, fmt.Errorf("reading %s@%s: %w", opts.Module, v, err)
		}
	}
	if len(r.Missing) > 0 {
		return r, nil
	}
	r.Compared = true
	byUse := map[string]int{}
	for _, u := range uses {
		byUse[u.Symbol]++
	}
	for _, c := range Diff(apis[0], apis[1]) {
		switch c.Kind {
		case "added":
			r.Added++
			continue
		case "removed":
			r.Removed++
		default:
			r.Changed++
		}
		if n := byUse[c.Symbol]; n > 0 {
			c.Uses = n
			r.Breaking = append(r.Breaking, c)
		}
	}
	return r, nil
}

func inModule(path, mod string) bool {
	return path == mod || strings.HasPrefix(path, mod+"/")
}

// currentVersion is the module's version in the build list, read from any
// package of it the repository imports.
func currentVersion(pkgs []*packages.Package, mod string) string {
	v := ""
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if v != "" || p.Module == nil || p.Module.Path != mod {
			return
		}
		m := p.Module
		if m.Replace != nil {
			m = m.Replace
		}
		v = m.Version
	})
	return v
}

// FindUses lists every identifier in the repository's own files that refers
// to an object of the module: calls, types, constants, variables, method
// values and struct fields, including field names in composite literals.
func FindUses(pkgs []*packages.Package, root, mod string) []Use {
	seen := map[Use]bool{}
	var out []Use
	add := func(u Use) {
		if u.Symbol != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, p := range pkgs {
		if p.TypesInfo == nil || inModule(p.PkgPath, mod) {
			continue
		}
		for _, f := range p.Syntax {
			name := p.Fset.Position(f.Pos()).Filename
			rel, err := filepath.Rel(root, name)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue // generated into the build cache
			}
			fileUses(p, f, filepath.ToSlash(rel), mod, add)
		}
	}
	slices.SortFunc(out, func(a, b Use) int {
		return cmp.Or(cmp.Compare(a.Symbol, b.Symbol), cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.In, b.In))
	})
	return out
}

func fileUses(p *packages.Package, f *ast.File, rel, mod string, add func(Use)) {
	info := p.TypesInfo
	at := func(pos token.Pos, sym, in string) {
		add(Use{Symbol: sym, File: rel, Line: p.Fset.Position(pos).Line, In: in})
	}
	for _, d := range f.Decls {
		in := ""
		if fd, ok := d.(*ast.FuncDecl); ok {
			in = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				in = recvName(fd.Recv.List[0].Type) + "." + in
			}
		}
		keyed := map[*ast.Ident]bool{}
		ast.Inspect(d, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				// Field names in T{Field: v} are uses of T's fields.
				owner := namedOf(info.TypeOf(n))
				for _, e := range n.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok {
						if v, ok := info.Uses[id].(*types.Var); ok && v.IsField() {
							keyed[id] = true
							if owner != nil && v.Pkg() != nil && inModule(v.Pkg().Path(), mod) {
								at(id.Pos(), memberKey(owner, id.Name), in)
							}
						}
					}
				}
			case *ast.SelectorExpr:
				sel := info.Selections[n]
				if sel == nil {
					return true
				}
				obj := sel.Obj()
				if obj.Pkg() == nil || !inModule(obj.Pkg().Path(), mod) {
					return true
				}
				keyed[n.Sel] = true
				// A promoted member belongs to the embedded type, not the
				// one it was selected through.
				recv := sel.Recv()
				if fn, ok := obj.(*types.Func); ok {
					if r := fn.Signature().Recv(); r != nil {
						recv = r.Type()
					}
				} else if len(sel.Index()) > 1 {
					return true
				}
				if owner := namedOf(recv); owner != nil {
					at(n.Sel.Pos(), memberKey(owner, obj.Name()), in)
				}
			case *ast.Ident:
				if keyed[n] {
					return true
				}
				obj := info.Uses[n]
				if obj == nil || obj.Pkg() == nil || !inModule(obj.Pkg().Path(), mod) || obj.Parent() != obj.Pkg().Scope() {
					return true
				}
				at(n.Pos(), obj.Pkg().Path()+"."+obj.Name(), in)
			}
			return true
		})
	}
}

// namedOf is the declared type behind t, through one pointer.
func namedOf(t types.Type) *types.TypeName {
	if t == nil {
		return nil
	}
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if n, ok := types.Unalias(t).(*types.Named); ok {
		return n.Origin().Obj()
	}
	return nil
}

func memberKey(owner *types.TypeName, member string) string {
	if owner.Pkg() == nil {
		return ""
	}
	return owner.Pkg().Path() + "." + owner.Name() + "." + member
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func countSymbols(uses []Use) int {
	n := 0
	for i, u := range uses {
		if i == 0 || u.Symbol != uses[i-1].Symbol {
			n++
		}
	}
	return n
}

func countFiles(uses []Use) int {
	files := map[string]bool{}
	for _, u := range uses {
		files[u.File] = true
	}
	return len(files)
}

// modCache is where go keeps downloaded modules.
func modCache(ctx context.Context) (string, error) {
	env, err := golang.GoEnv(ctx, false)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "go", "env", "GOMODCACHE")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMODCACHE: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("go env reports no module cache")
	}
	return dir, nil
}

func moduleDir(cache, mod, version string) (string, error) {
	p, err := module.EscapePath(mod)
	if err != nil {
		return "", err
	}
	v, err := module.EscapeVersion(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, filepath.FromSlash(p)+"@"+v), nil
}
