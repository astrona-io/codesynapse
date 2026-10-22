package vuln

import (
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// Reachability levels, strongest first.
const (
	Called   = "called"
	Imported = "imported"
	Required = "required"
)

// Apply adds Module and Vulnerability nodes to g. externalCalls and
// externalImports come from the Go extractor (golang.Result): full names of
// called out-of-repo functions mapped to caller IDs, and out-of-repo import
// paths mapped to importing package IDs.
func Apply(repo string, g *model.Graph, modules []Module, vulns []Vulnerability, externalCalls, externalImports map[string][]string) {
	repoID := model.NodeID(repo, model.KindRepository, repo)
	eco := map[string]string{} // module key -> ecosystem
	for _, m := range modules {
		id := model.NodeID(repo, model.KindModule, m.Key())
		eco[m.Key()] = m.ecosystem()
		g.AddNode(model.Node{ID: id, Kind: model.KindModule, Repo: repo, Name: m.Path, QualifiedName: m.Path, Version: m.Version, Indirect: m.Indirect, Ecosystem: m.ecosystem(),
			Dev: m.Dev, License: m.License, LicenseSource: m.LicenseSource})
		g.AddEdge(model.Edge{From: repoID, To: id, Kind: model.EdgeDependsOn})
	}

	// Index called symbols by import path in OSV notation.
	calledByPkg := map[string]map[string][]string{}
	for full, callers := range externalCalls {
		pkg, sym := osvSymbol(full)
		if pkg == "" {
			continue
		}
		if calledByPkg[pkg] == nil {
			calledByPkg[pkg] = map[string][]string{}
		}
		calledByPkg[pkg][sym] = append(calledByPkg[pkg][sym], callers...)
	}

	for _, v := range vulns {
		mod := Module{Path: v.Module, Ecosystem: v.Ecosystem}
		vid := model.NodeID(repo, model.KindVulnerability, mod.Key()+"@"+v.ID)
		n := model.Node{
			ID: vid, Kind: model.KindVulnerability, Repo: repo, Name: v.ID, QualifiedName: v.ID,
			Summary: v.Summary, Severity: v.Severity, CWE: v.CWE, Aliases: v.Aliases,
			Package: v.Module, Version: v.Version, FixedVersion: v.Fixed, URL: v.URL, Reachability: Required, Ecosystem: v.Ecosystem,
		}
		modID := model.NodeID(repo, model.KindModule, mod.Key())
		if mod.ecosystem() != EcosystemGo {
			// npm/PyPI advisories rarely list symbols: a package imported by
			// any source file is "imported", otherwise "required".
			name := ImportName(mod)
			for imp := range externalImports {
				if strings.EqualFold(imp, name) || strings.HasPrefix(imp, name+"/") {
					n.Reachability = Imported
					break
				}
			}
			g.AddNode(n)
			g.AddEdge(model.Edge{From: vid, To: modID, Kind: model.EdgeAffects})
			continue
		}
		for pkg, syms := range v.Symbols {
			if len(externalImports[pkg]) > 0 && n.Reachability == Required {
				n.Reachability = Imported
			}
			called := calledByPkg[pkg]
			for sym, callers := range called {
				if len(syms) > 0 && !matchesSymbol(syms, sym) {
					continue
				}
				if len(syms) == 0 && len(callers) == 0 {
					continue
				}
				n.Reachability = Called
				extID := model.NodeID(repo, model.KindExternal, pkg+"."+sym)
				g.AddNode(model.Node{ID: extID, Kind: model.KindExternal, Repo: repo, Name: sym, QualifiedName: pkg + "." + sym, Package: pkg})
				g.AddEdge(model.Edge{From: vid, To: extID, Kind: model.EdgeAffects})
				for _, caller := range callers {
					g.AddEdge(model.Edge{From: caller, To: extID, Kind: model.EdgeCalls})
				}
			}
		}
		// A vulnerability without symbol data affects the whole module: any
		// import of one of its packages counts as imported.
		if len(v.Symbols) == 0 {
			for imp := range externalImports {
				if v.Module != StdlibPath && (imp == v.Module || strings.HasPrefix(imp, v.Module+"/")) {
					n.Reachability = Imported
					break
				}
			}
		}
		g.AddNode(n)
		g.AddEdge(model.Edge{From: vid, To: modID, Kind: model.EdgeAffects})
	}
}

// osvSymbol converts a types.Func full name into OSV notation:
// "net/http.Get" -> ("net/http", "Get"), "(*net/http.Client).Do" -> ("net/http", "Client.Do").
func osvSymbol(full string) (pkg, sym string) {
	if strings.HasPrefix(full, "(") {
		recv, method, ok := strings.Cut(strings.TrimPrefix(full, "("), ").")
		if !ok {
			return "", ""
		}
		recv = strings.TrimPrefix(recv, "*")
		i := strings.LastIndexByte(recv, '.')
		if i < 0 {
			return "", ""
		}
		typ := recv[i+1:]
		if j := strings.IndexByte(typ, '['); j >= 0 { // generic receiver
			typ = typ[:j]
		}
		return recv[:i], typ + "." + method
	}
	i := strings.LastIndexByte(full, '.')
	if i < 0 {
		return "", ""
	}
	return full[:i], full[i+1:]
}

func matchesSymbol(syms []string, sym string) bool {
	for _, s := range syms {
		if s == sym {
			return true
		}
	}
	return false
}
