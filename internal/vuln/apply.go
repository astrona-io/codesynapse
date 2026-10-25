package vuln

import (
	"cmp"
	"slices"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
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
// paths mapped to importing package IDs. built is every package compiled
// into the repository's packages and tests (nil when not loaded): an
// advisory whose packages are all missing from it is marked CodeAbsent.
func Apply(repo string, g *model.Graph, modules []Module, vulns []Vulnerability, externalCalls, externalImports map[string][]string, built map[string]bool) {
	repoID := model.NodeID(repo, model.KindRepository, repo)
	modules = installedVersions(modules)
	eco := map[string]string{} // module key -> ecosystem
	// Every installed version is a node. The preferred one (direct before
	// indirect, then the highest) keeps the plain key as its ID, as before;
	// the others are told apart by their version.
	idOf := map[string]string{}        // key@version -> node ID
	preferredID := map[string]string{} // key -> node ID of the preferred version
	preferredVersion := map[string]string{}
	for _, m := range modules {
		id := model.NodeID(repo, model.KindModule, m.Key())
		if _, seen := preferredID[m.Key()]; seen {
			id = model.NodeID(repo, model.KindModule, m.Key()+"@"+m.Version)
		} else {
			preferredID[m.Key()], preferredVersion[m.Key()] = id, m.Version
		}
		idOf[m.Key()+"@"+m.Version] = id
	}
	for _, m := range modules {
		id := idOf[m.Key()+"@"+m.Version]
		eco[m.Key()] = m.ecosystem()
		g.AddNode(model.Node{ID: id, Kind: model.KindModule, Repo: repo, Name: m.Path, QualifiedName: m.Path, Version: m.Version, Indirect: m.Indirect, Ecosystem: m.ecosystem(),
			Dev: m.Dev, License: m.License, LicenseSource: m.LicenseSource, Via: m.Via, Roots: m.Roots,
			Latest: m.Latest, Deprecated: m.Deprecated, Behind: m.Behind,
			Summary: redact.Summary(m.replacementNote(), 160)})
		g.AddEdge(model.Edge{From: repoID, To: id, Kind: model.EdgeDependsOn})
	}
	// Module requires module, for the dependency graph of an SBOM. Edges to
	// modules the project does not have are dropped.
	have := make(map[string]bool, len(modules))
	for _, m := range modules {
		have[m.Key()] = true
	}
	for _, m := range modules {
		from := idOf[m.Key()+"@"+m.Version]
		// A requirement the lockfile resolved goes to the copy it names; the
		// rest go to the preferred version of the package.
		resolved := map[string]bool{}
		for _, r := range m.Resolved {
			if to, ok := idOf[r]; ok && to != from {
				g.AddEdge(model.Edge{From: from, To: to, Kind: model.EdgeDependsOn})
			}
			if i := strings.LastIndex(r, "@"); i > 0 { // the version follows the last @ ("npm:@scope/p@1.0.0")
				resolved[r[:i]] = true
			}
		}
		for _, r := range m.Requires {
			if r != m.Key() && have[r] && !resolved[r] {
				g.AddEdge(model.Edge{From: from, To: preferredID[r], Kind: model.EdgeDependsOn})
			}
		}
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
		// An advisory that hits a version other than the preferred one is a
		// node of its own, or both versions would share it.
		vkey := mod.Key() + "@" + v.ID
		if _, known := idOf[mod.Key()+"@"+v.Version]; known && v.Version != preferredVersion[mod.Key()] {
			vkey = mod.Key() + "@" + v.Version + "@" + v.ID
		}
		vid := model.NodeID(repo, model.KindVulnerability, vkey)
		n := model.Node{
			ID: vid, Kind: model.KindVulnerability, Repo: repo, Name: v.ID, QualifiedName: v.ID,
			Summary: v.Summary, Severity: v.Severity, CWE: v.CWE, Aliases: v.Aliases,
			Package: v.Module, Version: v.Version, FixedVersion: v.Fixed, URL: v.URL, Reachability: Required, Ecosystem: v.Ecosystem,
		}
		modID := cmp.Or(idOf[mod.Key()+"@"+v.Version], model.NodeID(repo, model.KindModule, mod.Key()))
		if mod.ecosystem() != EcosystemGo {
			// npm/PyPI advisories rarely list symbols: a package imported by
			// any source file is "imported", otherwise "required".
			if ImportedBy(mod, externalImports) {
				n.Reachability = Imported
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
		// With the build's packages known (nil: not loaded), an advisory
		// whose packages none of them is compiled in cannot run here.
		if built != nil && len(v.Symbols) > 0 && n.Reachability != Called {
			n.CodeAbsent = true
			for pkg := range v.Symbols {
				if built[pkg] {
					n.CodeAbsent = false
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

// installedVersions keeps one module per package version, in the same order
// on every run: lockfiles can hold several versions of a package (npm nests
// them), and they arrive in map order. Within a package a direct dependency
// comes before an indirect one, then the highest version first, so the first
// of each package is the preferred one. The same version found twice is one
// module, direct if any copy is.
func installedVersions(modules []Module) []Module {
	out := slices.Clone(modules)
	slices.SortStableFunc(out, func(a, b Module) int {
		return cmp.Or(strings.Compare(a.Key(), b.Key()), boolOrder(a.Indirect, b.Indirect), -compareVersions(a.Version, b.Version), strings.Compare(a.Version, b.Version))
	})
	return slices.CompactFunc(out, func(a, b Module) bool { return a.Key() == b.Key() && a.Version == b.Version })
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// compareVersions orders semantic versions as such, anything else as text.
func compareVersions(a, b string) int {
	va, vb := "v"+strings.TrimPrefix(a, "v"), "v"+strings.TrimPrefix(b, "v")
	if semver.IsValid(va) && semver.IsValid(vb) {
		return semver.Compare(va, vb)
	}
	return strings.Compare(a, b)
}

// ImportedBy reports whether any source file imports the (non-Go) module.
func ImportedBy(mod Module, externalImports map[string][]string) bool {
	name := ImportName(mod)
	for imp := range externalImports {
		// A C# using directive names a namespace, which a NuGet ID usually
		// prefixes (Newtonsoft.Json.Linq).
		if strings.EqualFold(imp, name) || strings.HasPrefix(imp, name+"/") ||
			mod.Ecosystem == EcosystemNuGet && strings.HasPrefix(strings.ToLower(imp), strings.ToLower(name)+".") ||
			mod.Ecosystem == EcosystemRubyGems && rubyRequires(imp, name) {
			return true
		}
	}
	return false
}

// rubyRequires reports whether require "imp" loads gem name: rack and
// rack/utils, or net-http as net/http.
func rubyRequires(imp, name string) bool {
	for _, n := range []string{name, strings.ReplaceAll(name, "-", "/")} {
		if strings.EqualFold(imp, n) || strings.HasPrefix(imp, n+"/") {
			return true
		}
	}
	return false
}
