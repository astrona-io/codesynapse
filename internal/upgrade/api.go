package upgrade

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// The exported API of a module version, read from its source with go/parser
// alone: a module in the cache has no build list of its own here, and type
// checking it would need its dependencies. Declarations are compared as
// printed text with parameter names dropped, so renaming a parameter is not
// a change and anything else in a signature or field type is.

const (
	maxAPIFiles     = 20000
	maxAPIFileBytes = 2 << 20
)

// API maps every exported declaration of the module in dir to its printed
// form, keyed like Use.Symbol. Internal packages, tests, testdata, vendored
// code and nested modules are left out: no importer can see them. A
// declaration that differs between build-tagged files keeps every variant.
func API(dir, mod string) (map[string]string, error) {
	out := map[string]string{}
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			name := d.Name()
			if p != dir {
				if name == "internal" || name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}
				if _, err := os.Lstat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if files++; files > maxAPIFiles {
			return fmt.Errorf("more than %d Go files", maxAPIFiles)
		}
		if st, err := d.Info(); err != nil || st.Size() > maxAPIFileBytes {
			return err
		}
		pkg := mod
		if d := path.Dir(rel); d != "." {
			pkg = mod + "/" + d
		}
		return declare(p, pkg, out)
	})
	return out, err
}

func declare(file, pkg string, out map[string]string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil //nolint:nilerr // a file go would not build adds nothing
	}
	if f.Name.Name == "main" || strings.HasSuffix(f.Name.Name, "_test") {
		return nil
	}
	put := func(key, val string) {
		if old, ok := out[key]; ok && old != val {
			vs := append(strings.Split(old, " | "), val)
			slices.Sort(vs)
			val = strings.Join(slices.Compact(vs), " | ")
		}
		out[key] = val
	}
	text := func(n ast.Node) string {
		var b bytes.Buffer
		_ = printer.Fprint(&b, fset, n)
		return strings.Join(strings.Fields(b.String()), " ")
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			sig := text(unnamed(d.Type))
			if d.Recv == nil || len(d.Recv.List) == 0 {
				put(pkg+"."+d.Name.Name, sig)
				continue
			}
			if r := recvName(d.Recv.List[0].Type); ast.IsExported(r) {
				_, ptr := d.Recv.List[0].Type.(*ast.StarExpr)
				put(pkg+"."+r+"."+d.Name.Name, map[bool]string{true: "(*) ", false: ""}[ptr]+sig)
			}
		case *ast.GenDecl:
			var lastType ast.Expr
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						typeDecl(pkg+"."+s.Name.Name, s, text, put)
					}
				case *ast.ValueSpec:
					// In a const block a spec without a type or values
					// repeats the one before it (iota).
					if d.Tok == token.CONST && (s.Type != nil || len(s.Values) > 0) {
						lastType = s.Type
					}
					typ := s.Type
					if d.Tok == token.CONST && typ == nil && len(s.Values) == 0 {
						typ = lastType
					}
					val := d.Tok.String()
					if typ != nil {
						val += " " + text(typ)
					}
					for _, n := range s.Names {
						if n.IsExported() {
							put(pkg+"."+n.Name, val)
						}
					}
				}
			}
		}
	}
	return nil
}

func typeDecl(key string, s *ast.TypeSpec, text func(ast.Node) string, put func(string, string)) {
	head := "type"
	if s.TypeParams != nil {
		// Printed through a function type, which the printer accepts.
		sig := text(&ast.FuncType{TypeParams: s.TypeParams, Params: &ast.FieldList{}})
		head += strings.TrimSuffix(strings.TrimPrefix(sig, "func"), "()")
	}
	if s.Assign.IsValid() {
		put(key, head+" = "+text(s.Type))
		return
	}
	switch t := s.Type.(type) {
	case *ast.StructType:
		var embeds []string
		for _, f := range t.Fields.List {
			if len(f.Names) == 0 {
				embeds = append(embeds, text(f.Type))
				if n := recvName(f.Type); ast.IsExported(n) {
					put(key+"."+n, "field "+text(f.Type))
				}
				continue
			}
			for _, n := range f.Names {
				if n.IsExported() {
					put(key+"."+n.Name, "field "+text(f.Type))
				}
			}
		}
		put(key, head+" struct{"+strings.Join(embeds, "; ")+"}")
	case *ast.InterfaceType:
		var embeds []string
		for _, m := range t.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok || len(m.Names) == 0 {
				embeds = append(embeds, text(m.Type))
				continue
			}
			for _, n := range m.Names {
				// Unexported methods still decide who can implement it.
				if n.IsExported() {
					put(key+"."+n.Name, text(unnamed(ft)))
				} else {
					embeds = append(embeds, n.Name+strings.TrimPrefix(text(unnamed(ft)), "func"))
				}
			}
		}
		slices.Sort(embeds)
		put(key, head+" interface{"+strings.Join(embeds, "; ")+"}")
	default:
		put(key, head+" "+text(s.Type))
	}
}

// unnamed is the function type without parameter and result names.
func unnamed(ft *ast.FuncType) *ast.FuncType {
	// Type parameters keep their names: the signature refers to them.
	out := &ast.FuncType{TypeParams: ft.TypeParams, Params: unnamedList(ft.Params)}
	if ft.Results != nil && len(ft.Results.List) > 0 {
		out.Results = unnamedList(ft.Results)
	}
	return out
}

// unnamedList repeats each field's type once per name it had.
func unnamedList(fl *ast.FieldList) *ast.FieldList {
	out := &ast.FieldList{}
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		for range max(1, len(f.Names)) {
			out.List = append(out.List, &ast.Field{Type: f.Type})
		}
	}
	return out
}

// Diff lists what changed from old to next, sorted by symbol.
func Diff(old, next map[string]string) []Change {
	var out []Change
	for k, o := range old {
		switch n, ok := next[k]; {
		case !ok:
			out = append(out, Change{Symbol: k, Kind: "removed", Old: o})
		case n != o:
			out = append(out, Change{Symbol: k, Kind: "changed", Old: o, New: n})
		}
	}
	for k, n := range next {
		if _, ok := old[k]; !ok {
			out = append(out, Change{Symbol: k, Kind: "added", New: n})
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return cmp.Compare(a.Symbol, b.Symbol) })
	return out
}
