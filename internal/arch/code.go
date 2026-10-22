package arch

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// codeIndex finds the code a record names: a file path, a directory (its
// packages), a package path or suffix, or a symbol by qualified or short name.
// A name that matches several symbols is reported, never guessed.
type codeIndex struct {
	files    map[string]string   // path -> File node
	dirs     map[string][]string // directory -> Package nodes of the files directly in it
	packages map[string][]string // import path -> Package node
	symbols  map[string][]string // qualified and short names -> nodes
}

func newCodeIndex(g *model.Graph) *codeIndex {
	ix := &codeIndex{files: map[string]string{}, dirs: map[string][]string{}, packages: map[string][]string{}, symbols: map[string][]string{}}
	pkgByName := map[string]string{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindPackage {
			pkgByName[n.QualifiedName] = n.ID
			ix.packages[n.QualifiedName] = append(ix.packages[n.QualifiedName], n.ID)
		}
	}
	for _, n := range g.Nodes() {
		switch n.Kind {
		case model.KindFile:
			ix.files[n.File] = n.ID
			if p, ok := pkgByName[n.Package]; ok {
				dir := path.Dir(n.File)
				if !slices.Contains(ix.dirs[dir], p) {
					ix.dirs[dir] = append(ix.dirs[dir], p)
				}
			}
		case model.KindFunction, model.KindMethod, model.KindType, model.KindInterfaceMethod:
			if n.IsTest {
				continue
			}
			ix.symbols[n.QualifiedName] = append(ix.symbols[n.QualifiedName], n.ID)
			// The package-qualified and the bare name too; a bare name that many
			// symbols share is reported as ambiguous by resolve, never guessed.
			for _, alias := range []string{n.ShortName(), n.Name} {
				if alias != n.QualifiedName && alias != "" && !slices.Contains(ix.symbols[alias], n.ID) {
					ix.symbols[alias] = append(ix.symbols[alias], n.ID)
				}
			}
		}
	}
	return ix
}

// resolve returns the nodes ref names and, when it names none or several, a
// sentence saying so.
func (ix *codeIndex) resolve(ref string) ([]string, string) {
	ref = strings.TrimSpace(strings.Trim(ref, "`"))
	if ref == "" {
		return nil, ""
	}
	clean := strings.TrimPrefix(path.Clean(strings.TrimSuffix(ref, "/")), "./")
	if id, ok := ix.files[clean]; ok {
		return []string{id}, ""
	}
	if ps, ok := ix.dirs[clean]; ok {
		return slices.Clone(ps), ""
	}
	if ids, ok := ix.symbols[ref]; ok {
		if len(ids) == 1 {
			return ids, ""
		}
		return nil, fmt.Sprintf("implements: %q matches %d symbols; use a qualified name", ref, len(ids))
	}
	if ids := ix.packages[ref]; len(ids) == 1 {
		return ids, ""
	}
	var hits []string
	for name, ids := range ix.packages {
		if strings.HasSuffix(name, "/"+clean) {
			hits = append(hits, ids...)
		}
	}
	slices.Sort(hits)
	hits = slices.Compact(hits)
	switch len(hits) {
	case 1:
		return hits, ""
	case 0:
		return nil, fmt.Sprintf("implements: nothing in the code is called %q (file, directory, package or symbol)", ref)
	}
	return nil, fmt.Sprintf("implements: %q matches %d packages; use a longer path", ref, len(hits))
}
