package golang

import (
	"fmt"
	"go/ast"
	"go/types"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
)

// A function is a clone candidate only when it is big enough that sharing it
// is worth the trouble; getters and one-line wrappers repeat everywhere.
const (
	minCloneStmts = 5
	minCloneLOC   = 6
)

// cloneHash fingerprints the structure of d: the shape of its signature and
// body with local names and literal values abstracted, so copies that differ
// only in naming or constants hash the same. Everything that carries meaning
// stays: called functions, types, fields, imported packages and operators.
// It returns "" when d is too small to be a candidate.
func cloneHash(info *types.Info, d *ast.FuncDecl) (hash, sig string) {
	if d.Body == nil {
		return "", ""
	}
	w := &cloneWalk{info: info, locals: map[types.Object]int{}}
	// The receiver's type is left out so methods of different types can match;
	// its name is a local like any parameter.
	if d.Recv != nil {
		for _, f := range d.Recv.List {
			for _, n := range f.Names {
				w.local(info.Defs[n])
			}
		}
	}
	w.walk(d.Type)
	w.walk(d.Body)
	if w.stmts < minCloneStmts {
		return "", ""
	}
	return duplicates.Hash(w.toks), duplicates.Sig(w.toks)
}

type cloneWalk struct {
	info   *types.Info
	toks   []string
	locals map[types.Object]int
	stmts  int
}

func (w *cloneWalk) local(o types.Object) {
	if o != nil {
		if _, ok := w.locals[o]; !ok {
			w.locals[o] = len(w.locals)
		}
	}
}

func (w *cloneWalk) emit(format string, args ...any) {
	w.toks = append(w.toks, fmt.Sprintf(format, args...))
}

func (w *cloneWalk) walk(n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		if n == nil {
			w.emit(")")
			return true
		}
		switch n := n.(type) {
		case *ast.Ident:
			w.emit("id:%s", w.ident(n))
			return true
		case *ast.BasicLit:
			w.emit("lit:%s", n.Kind)
			return true
		case *ast.BinaryExpr:
			w.emit("bin:%s", n.Op)
		case *ast.UnaryExpr:
			w.emit("un:%s", n.Op)
		case *ast.AssignStmt:
			w.emit("asg:%s", n.Tok)
		case *ast.IncDecStmt:
			w.emit("incdec:%s", n.Tok)
		case *ast.BranchStmt:
			w.emit("br:%s", n.Tok)
		case *ast.RangeStmt:
			w.emit("rng:%s", n.Tok)
		case *ast.GenDecl:
			w.emit("gen:%s", n.Tok)
		}
		if _, ok := n.(ast.Stmt); ok {
			if _, block := n.(*ast.BlockStmt); !block {
				w.stmts++
			}
		}
		w.emit("%T(", n)
		return true
	})
}

// ident names what an identifier refers to without its spelling: locals by
// order of first appearance, everything else by package and name.
func (w *cloneWalk) ident(id *ast.Ident) string {
	obj := w.info.ObjectOf(id)
	switch o := obj.(type) {
	case nil:
		return "?" // labels, blank identifiers, unresolved names
	case *types.Var:
		if o.IsField() {
			return "field." + o.Name()
		}
		if o.Pkg() != nil && o.Parent() == o.Pkg().Scope() {
			return o.Pkg().Path() + "." + o.Name()
		}
		w.local(obj)
		return fmt.Sprintf("v%d", w.locals[obj])
	case *types.PkgName:
		return "pkg." + o.Imported().Path()
	}
	if obj.Pkg() == nil {
		return "universe." + obj.Name()
	}
	return obj.Pkg().Path() + "." + obj.Name()
}
