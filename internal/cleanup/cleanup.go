// Package cleanup finds code, dependencies and configuration that nothing
// uses. Unused code is a security cost: it is compiled, shipped and must be
// reviewed and patched, can be revived by accident, and can be the only
// thing pulling a vulnerable dependency into the build.
//
// Liveness is computed by reachability over the complete graph at ingest:
// roots are main and init functions, exported API outside internal/ and
// main packages, methods the standard library calls implicitly (String,
// Error, ServeHTTP, ...), and package-level initialisers. Everything
// reachable from a root over CALLS, DISPATCHES_TO and REFERENCES is live.
// Code only reachable from tests is test-only; the rest is unreachable.
package cleanup

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/vuln"
)

// Liveness values.
const (
	Live        = "live"
	TestOnly    = "test-only"
	Unreachable = "unreachable"
	Used        = "used"
	Unused      = "unused"
	// TestSupport marks functions of packages only tests use.
	TestSupport = "test-support"
)

// Confidence that an item is really unused.
const (
	High   = "high"   // unexported function: the compiler would agree
	Medium = "medium" // unexported method, or exported symbol inside internal/ or main
	Low    = "low"    // may be called by reflection, plugins or an interface defined elsewhere
)

// implicitMethods are invoked by the standard library or common frameworks
// through interfaces CodeSynapse cannot see in the repository.
var implicitMethods = map[string]bool{
	"String": true, "GoString": true, "Error": true, "Unwrap": true, "Is": true, "As": true, "Format": true,
	"ServeHTTP": true, "MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true, "UnmarshalText": true,
	"MarshalYAML": true, "UnmarshalYAML": true, "MarshalBinary": true, "UnmarshalBinary": true,
	"Scan": true, "Value": true, "Len": true, "Less": true, "Swap": true, "Push": true, "Pop": true,
	"Read": true, "Write": true, "Close": true, "Seek": true, "ReadFrom": true, "WriteTo": true,
	"Run": true, "Execute": true, "Reset": true, "ProtoMessage": true, "LogValue": true,
	"Handle": true, "Enabled": true, "WithAttrs": true, "WithGroup": true,
}

var liveEdges = map[model.EdgeKind]bool{model.EdgeCalls: true, model.EdgeDispatchesTo: true, model.EdgeReferences: true}

// Item is one unused element.
type Item struct {
	query.Ref
	Category   string   `json:"category"`
	Confidence string   `json:"confidence"`
	LOC        int      `json:"loc"`
	Security   []string `json:"security,omitempty"`
	Reason     string   `json:"reason"`
}

// FileItem is a Go file in which nothing is live.
type FileItem struct {
	File     string `json:"file"`
	Symbols  int    `json:"symbols"`
	LOC      int    `json:"loc"`
	TestOnly bool   `json:"test_only"`
}

// ModuleItem is a direct go.mod requirement no package imports.
type ModuleItem struct {
	Path string `json:"path"`
	// Ecosystem is npm or PyPI for those; empty for a Go module.
	Ecosystem string   `json:"ecosystem,omitempty"`
	Version   string   `json:"version"`
	Vulns     []string `json:"vulnerabilities,omitempty"`
}

// Summary counts the report.
type Summary struct {
	Functions        int     `json:"functions"`
	Live             int     `json:"live"`
	TestOnly         int     `json:"test_only"`
	Unreachable      int     `json:"unreachable"`
	DeadLOC          int     `json:"dead_loc"`
	DeadPct          float64 `json:"dead_pct"`
	SecurityRelevant int     `json:"security_relevant"`
	UnusedModules    int     `json:"unused_modules"`
	CVEsRemovable    int     `json:"cves_removable"`
	DeadFiles        int     `json:"dead_files"`
	UnusedValues     int     `json:"unused_values"`
	UnusedDefines    int     `json:"unused_template_defines"`
	TestSupportPkgs  int     `json:"test_support_packages"`
}

// Report is the cleanup analysis of one repository.
type Report struct {
	Summary       Summary      `json:"summary"`
	Items         []Item       `json:"items"`
	Files         []FileItem   `json:"files"`
	Modules       []ModuleItem `json:"modules"`
	UnusedValues  []query.Ref  `json:"unused_values"`
	UnusedDefines []query.Ref  `json:"unused_template_defines"`
	// TestSupport lists packages only tests use (not counted as dead).
	TestSupport []string `json:"test_support_packages"`
}

// Analyze computes liveness, sets Node.Liveness on functions, methods and
// modules in g, and returns the report. externalImports maps imported
// out-of-repo package paths to importers (golang.Result.ExternalImports).
func Analyze(g *model.Graph, externalImports map[string][]string) Report {
	nodes := g.Nodes()
	out := map[string][]model.Edge{}
	in := map[string][]model.Edge{}
	for _, e := range g.Edges() {
		out[e.From] = append(out[e.From], e)
		in[e.To] = append(in[e.To], e)
	}
	byID := make(map[string]model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}

	var roots, tests []string
	for _, n := range nodes {
		switch {
		case n.Kind == model.KindFile && !n.IsTest:
			roots = append(roots, n.ID) // package-level initialisers
		case !isCallable(n):
		case isTestCode(n):
			tests = append(tests, n.ID)
		case isRoot(n):
			roots = append(roots, n.ID)
		}
	}
	live := reach(roots, out)
	tested := reach(tests, out)

	rep := Report{Items: []Item{}, Files: []FileItem{}, Modules: []ModuleItem{}, UnusedValues: []query.Ref{}, UnusedDefines: []query.Ref{}, TestSupport: []string{}}
	type fileAcc struct {
		symbols, dead, loc int
		testOnly           bool
	}
	files := map[string]*fileAcc{}
	totalLOC := 0
	// A package whose functions are only ever reached from tests is test
	// support (fixtures, assertion helpers): it never ships, so it is not
	// dead code. Find those packages first.
	pkgLive := map[string]bool{}
	for _, n := range nodes {
		if isCallable(n) && !isTestCode(n) && live[n.ID] {
			pkgLive[n.Package] = true
		}
	}
	testSupport := map[string]bool{}
	for _, n := range nodes {
		if isCallable(n) && !isTestCode(n) && !pkgLive[n.Package] && tested[n.ID] {
			testSupport[n.Package] = true
		}
	}
	for p := range testSupport {
		rep.TestSupport = append(rep.TestSupport, p)
	}
	slices.Sort(rep.TestSupport)
	rep.Summary.TestSupportPkgs = len(rep.TestSupport)

	for _, n := range nodes {
		if !isCallable(n) || isTestCode(n) {
			continue
		}
		if testSupport[n.Package] {
			if gn, ok := g.Node(n.ID); ok {
				gn.Liveness = TestSupport
			}
			continue
		}
		state := Live
		switch {
		case live[n.ID]:
		case tested[n.ID]:
			state = TestOnly
		default:
			state = Unreachable
		}
		if gn, ok := g.Node(n.ID); ok {
			gn.Liveness = state
		}
		rep.Summary.Functions++
		totalLOC += n.LOC
		fa := files[n.File]
		if fa == nil {
			fa = &fileAcc{testOnly: true}
			files[n.File] = fa
		}
		fa.symbols++
		if state == Live {
			rep.Summary.Live++
			fa.testOnly = false
			continue
		}
		fa.dead++
		fa.loc += n.LOC
		if state == TestOnly {
			rep.Summary.TestOnly++
		} else {
			rep.Summary.Unreachable++
			fa.testOnly = false
		}
		rep.Summary.DeadLOC += n.LOC
		item := Item{Ref: query.RefOf(n), Category: state, Confidence: confidence(n), LOC: n.LOC, Security: securityOf(n, out, in, byID)}
		item.Reason = reason(n, state)
		if len(item.Security) > 0 {
			rep.Summary.SecurityRelevant++
		}
		rep.Items = append(rep.Items, item)
	}
	if totalLOC > 0 {
		rep.Summary.DeadPct = float64(int(float64(rep.Summary.DeadLOC)/float64(totalLOC)*1000)) / 10
	}
	for file, fa := range files {
		if fa.symbols > 0 && fa.dead == fa.symbols {
			rep.Files = append(rep.Files, FileItem{File: file, Symbols: fa.symbols, LOC: fa.loc, TestOnly: fa.testOnly})
		}
	}
	rep.Summary.DeadFiles = len(rep.Files)

	rep.Modules = unusedModules(g, nodes, in, externalImports)
	rep.Summary.UnusedModules = len(rep.Modules)
	for _, m := range rep.Modules {
		rep.Summary.CVEsRemovable += len(m.Vulns)
	}
	rep.UnusedValues, rep.UnusedDefines = unusedConfig(nodes, in)
	rep.Summary.UnusedValues, rep.Summary.UnusedDefines = len(rep.UnusedValues), len(rep.UnusedDefines)

	// Security-relevant first, then high confidence, then size.
	slices.SortFunc(rep.Items, func(a, b Item) int {
		return cmp.Or(
			cmp.Compare(len(b.Security), len(a.Security)),
			cmp.Compare(confRank(b.Confidence), confRank(a.Confidence)),
			cmp.Compare(b.LOC, a.LOC),
			cmp.Compare(a.Name, b.Name),
		)
	})
	slices.SortFunc(rep.Files, func(a, b FileItem) int { return cmp.Or(cmp.Compare(b.LOC, a.LOC), cmp.Compare(a.File, b.File)) })
	return rep
}

// Encode returns the report as JSON for the Repository node.
func Encode(r Report) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func isCallable(n model.Node) bool { return n.Kind == model.KindFunction || n.Kind == model.KindMethod }

// isTestCode covers test functions and every helper declared in test files.
func isTestCode(n model.Node) bool {
	return n.IsTest || strings.HasSuffix(n.File, "_test.go") || strings.Contains(n.File, "/testdata/")
}

func internalOrMain(n model.Node) bool {
	return strings.Contains(n.Package+"/", "/internal/") || strings.HasSuffix(n.Package, "/main") || isMainPkg(n)
}

// isMainPkg is a heuristic: main packages live under cmd/ or at the root of
// a command; the extractor stores package paths, not names.
func isMainPkg(n model.Node) bool {
	return strings.Contains(n.Package+"/", "/cmd/")
}

func isRoot(n model.Node) bool {
	switch {
	case n.Name == "main" || n.Name == "init":
		return true
	case n.Kind == model.KindMethod && implicitMethods[n.Name]:
		return true
	case n.Exported && !internalOrMain(n):
		return true // public API of a library package
	}
	return false
}

func reach(roots []string, out map[string][]model.Edge) map[string]bool {
	seen := make(map[string]bool, len(roots))
	stack := slices.Clone(roots)
	for _, r := range roots {
		seen[r] = true
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range out[cur] {
			if liveEdges[e.Kind] && !seen[e.To] {
				seen[e.To] = true
				stack = append(stack, e.To)
			}
		}
	}
	return seen
}

func confidence(n model.Node) string {
	switch {
	case !n.Exported && n.Kind == model.KindFunction:
		return High
	case !n.Exported, n.Exported && internalOrMain(n) && n.Kind == model.KindFunction:
		return Medium
	default:
		return Low
	}
}

func confRank(c string) int { return map[string]int{Low: 0, Medium: 1, High: 2}[c] }

func reason(n model.Node, state string) string {
	if state == TestOnly {
		return "only reached from tests: production code never calls it"
	}
	if n.Exported {
		return "exported but nothing in the repository calls or references it"
	}
	return "no caller, reference or interface dispatch reaches it"
}

// securityOf lists why removing n reduces risk: its own security findings,
// sensitive APIs it calls, and vulnerable symbols it is a caller of.
func securityOf(n model.Node, out, in map[string][]model.Edge, byID map[string]model.Node) []string {
	var s []string
	for _, t := range n.SecurityTags {
		s = append(s, "calls "+t+" API")
	}
	for _, e := range out[n.ID] {
		t := byID[e.To]
		switch {
		case e.Kind == model.EdgeHasFinding:
			s = append(s, t.Severity+" finding: "+t.Rule)
		case e.Kind == model.EdgeCalls && t.Kind == model.KindExternal && affected(in[t.ID]):
			s = append(s, "calls vulnerable "+t.QualifiedName)
		}
	}
	return s
}

func unusedModules(g *model.Graph, nodes []model.Node, in map[string][]model.Edge, imports map[string][]string) []ModuleItem {
	var out []ModuleItem
	for _, n := range nodes {
		if n.Kind != model.KindModule || n.Name == "stdlib" {
			continue
		}
		used := false
		switch n.Ecosystem {
		case "", "Go":
			for imp := range imports {
				if imp == n.Name || strings.HasPrefix(imp, n.Name+"/") {
					used = true
					break
				}
			}
		case vuln.EcosystemNPM, vuln.EcosystemPyPI:
			// Tooling (bundlers, linters, test runners, type packages, CLIs)
			// is used without an import, so only a runtime dependency that
			// is not one of those is judged, and only when some source was
			// parsed at all (a repository whose files we could not read has
			// no imports to compare with).
			if n.Dev || n.Indirect || len(imports) == 0 || toolingDependency(n.Ecosystem, n.Name) {
				continue
			}
			used = vuln.ImportedBy(vuln.Module{Path: n.Name, Ecosystem: n.Ecosystem}, imports)
		default:
			continue
		}
		state := Used
		if !used {
			state = Unused
		}
		if gn, ok := g.Node(n.ID); ok {
			gn.Liveness = state
		}
		// Indirect requirements are needed by other dependencies; only
		// direct ones are the repository's choice to remove.
		if used || n.Indirect {
			continue
		}
		m := ModuleItem{Path: n.Name, Version: n.Version}
		if n.Ecosystem != "" && n.Ecosystem != "Go" {
			m.Ecosystem = n.Ecosystem
		}
		for _, e := range in[n.ID] {
			if e.Kind == model.EdgeAffects {
				if v, ok := g.Node(e.From); ok && v.Kind == model.KindVulnerability {
					m.Vulns = append(m.Vulns, v.Name)
				}
			}
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b ModuleItem) int {
		return cmp.Or(cmp.Compare(len(b.Vulns), len(a.Vulns)), cmp.Compare(a.Path, b.Path))
	})
	if out == nil {
		out = []ModuleItem{}
	}
	return out
}

// unusedConfig finds Helm values keys no template reads (leaf keys whose
// path and ancestors have no USES_VALUE edge) and template defines nothing
// includes.
func unusedConfig(nodes []model.Node, in map[string][]model.Edge) ([]query.Ref, []query.Ref) {
	usedPaths := map[string]map[string]bool{} // values file -> used key paths
	valuesFiles := map[string]bool{}
	for _, n := range nodes {
		if n.Kind != model.KindConfigEntry {
			continue
		}
		for _, e := range in[n.ID] {
			if e.Kind == model.EdgeUsesValue {
				if usedPaths[n.File] == nil {
					usedPaths[n.File] = map[string]bool{}
				}
				usedPaths[n.File][n.Name] = true
				valuesFiles[n.File] = true
			}
		}
	}
	values, defines := []query.Ref{}, []query.Ref{}
	for _, n := range nodes {
		switch n.Kind {
		case model.KindConfigEntry:
			if !valuesFiles[n.File] || n.Signature == "map" {
				continue // only files some template reads; report leaves
			}
			if !pathUsed(n.Name, usedPaths[n.File]) {
				values = append(values, query.RefOf(n))
			}
		case model.KindTemplateDef:
			included := false
			for _, e := range in[n.ID] {
				if e.Kind == model.EdgeIncludes {
					included = true
					break
				}
			}
			if !included {
				defines = append(defines, query.RefOf(n))
			}
		}
	}
	byLoc := func(a, b query.Ref) int { return cmp.Compare(a.Location, b.Location) }
	slices.SortFunc(values, byLoc)
	slices.SortFunc(defines, byLoc)
	return values, defines
}

// pathUsed reports whether path, an ancestor (a template reads the whole
// map) or a descendant is used.
func pathUsed(path string, used map[string]bool) bool {
	for u := range used {
		if u == path || strings.HasPrefix(path, u+".") || strings.HasPrefix(u, path+".") {
			return true
		}
	}
	return false
}

func affected(edges []model.Edge) bool {
	for _, e := range edges {
		if e.Kind == model.EdgeAffects {
			return true
		}
	}
	return false
}
