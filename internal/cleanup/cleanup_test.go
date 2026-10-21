package cleanup

import (
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestAnalyze(t *testing.T) {
	g := model.NewGraph()
	id := func(k model.NodeKind, l string) string { return model.NodeID("r", k, l) }
	fn := func(name string, exported, test bool, pkg string, extra ...func(*model.Node)) string {
		n := model.Node{ID: id(model.KindFunction, pkg+"."+name), Kind: model.KindFunction, Repo: "r", Name: name,
			QualifiedName: pkg + "." + name, Package: pkg, File: "f.go", Exported: exported, IsTest: test, LOC: 10, StartLine: 1, EndLine: 10}
		for _, e := range extra {
			e(&n)
		}
		g.AddNode(n)
		return n.ID
	}
	edge := func(a, b string, k model.EdgeKind) { g.AddEdge(model.Edge{From: a, To: b, Kind: k}) }
	const app = "example.com/x/cmd/app"
	main := fn("main", false, false, app)
	run := fn("run", false, false, app)
	handler := fn("handler", false, false, app)
	helper := fn("helper", false, false, app)
	testOnly := fn("fixture", false, false, app)
	test := fn("TestRun", true, true, app)
	dead := fn("legacyExec", false, false, app, func(n *model.Node) { n.SecurityTags = []string{"exec"} })
	api := fn("Public", true, false, "example.com/x/pkg/lib")

	edge(main, run, model.EdgeCalls)
	edge(run, handler, model.EdgeReferences) // passed as a callback
	edge(test, testOnly, model.EdgeCalls)
	edge(test, run, model.EdgeCalls)
	finding := id(model.KindFinding, "f.go:3:exec-shell")
	g.AddNode(model.Node{ID: finding, Kind: model.KindFinding, Repo: "r", Rule: "exec-shell", Severity: "critical"})
	edge(dead, finding, model.EdgeHasFinding)
	_ = helper

	// Modules: one used, one unused with a CVE, one indirect.
	repoID := id(model.KindRepository, "r")
	g.AddNode(model.Node{ID: repoID, Kind: model.KindRepository, Repo: "r", Name: "r"})
	for _, m := range []model.Node{
		{Name: "golang.org/x/net", Version: "v1"}, {Name: "github.com/old/lib", Version: "v2"}, {Name: "github.com/ind/dep", Version: "v3", Indirect: true},
	} {
		m.ID, m.Kind, m.Repo = id(model.KindModule, m.Name), model.KindModule, "r"
		g.AddNode(m)
	}
	vuln := id(model.KindVulnerability, "github.com/old/lib@GO-1")
	g.AddNode(model.Node{ID: vuln, Kind: model.KindVulnerability, Repo: "r", Name: "GO-1"})
	edge(vuln, id(model.KindModule, "github.com/old/lib"), model.EdgeAffects)

	rep := Analyze(g, map[string][]string{"golang.org/x/net/http2": {"p"}})

	state := func(fid string) string { n, _ := g.Node(fid); return n.Liveness }
	assert.Equal(t, state(main), Live)
	assert.Equal(t, state(run), Live)
	assert.Equal(t, state(handler), Live, "referenced as a value")
	assert.Equal(t, state(api), Live, "exported API of a library package")
	assert.Equal(t, state(testOnly), TestOnly)
	assert.Equal(t, state(helper), Unreachable)
	assert.Equal(t, state(dead), Unreachable)

	require.Greater(t, len(rep.Items), 2)
	assert.Equal(t, rep.Items[0].Name, "app.legacyExec", "security-relevant dead code first")
	assert.Contains(t, rep.Items[0].Security, "critical finding: exec-shell")
	assert.Equal(t, rep.Items[0].Confidence, High)
	assert.Equal(t, rep.Summary.TestOnly, 1)
	assert.Equal(t, rep.Summary.Unreachable, 2)
	assert.Equal(t, rep.Summary.SecurityRelevant, 1)

	require.Len(t, rep.Modules, 1)
	assert.Equal(t, rep.Modules[0].Path, "github.com/old/lib")
	assert.Equal(t, rep.Modules[0].Vulns, []string{"GO-1"})
	assert.Equal(t, rep.Summary.CVEsRemovable, 1)
	m, _ := g.Node(id(model.KindModule, "golang.org/x/net"))
	assert.Equal(t, m.Liveness, Used)
}

func TestUnusedConfig(t *testing.T) {
	g := model.NewGraph()
	add := func(n model.Node) string { n.Repo = "r"; g.AddNode(n); return n.ID }
	tpl := add(model.Node{ID: "r|Template|t", Kind: model.KindTemplate})
	img := add(model.Node{ID: "r|ConfigEntry|v#image", Kind: model.KindConfigEntry, Name: "image", File: "values.yaml", Signature: "map"})
	repo := add(model.Node{ID: "r|ConfigEntry|v#image.repository", Kind: model.KindConfigEntry, Name: "image.repository", File: "values.yaml", Signature: "scalar"})
	unused := add(model.Node{ID: "r|ConfigEntry|v#debug", Kind: model.KindConfigEntry, Name: "debug", File: "values.yaml", Signature: "scalar"})
	def := add(model.Node{ID: "r|TemplateDef|x", Kind: model.KindTemplateDef, Name: "x"})
	g.AddEdge(model.Edge{From: tpl, To: repo, Kind: model.EdgeUsesValue})
	_, _ = img, def

	rep := Analyze(g, nil)
	require.Len(t, rep.UnusedValues, 1)
	assert.Equal(t, rep.UnusedValues[0].ID, unused)
	require.Len(t, rep.UnusedDefines, 1)
	assert.Equal(t, rep.UnusedDefines[0].Name, "x")
}

func TestTestSupportPackage(t *testing.T) {
	g := model.NewGraph()
	add := func(n model.Node) string { n.Repo = "r"; n.Kind = model.KindFunction; g.AddNode(n); return n.ID }
	test := add(model.Node{ID: "r|Function|p.TestX", Name: "TestX", Package: "p", File: "p/x_test.go", IsTest: true})
	helperInTest := add(model.Node{ID: "r|Function|p.mk", Name: "mk", Package: "p", File: "p/x_test.go"})
	assertEq := add(model.Node{ID: "r|Function|testutil.Equal", Name: "Equal", Exported: true, Package: "example.com/x/internal/testutil", File: "internal/testutil/a.go", LOC: 5})
	g.AddEdge(model.Edge{From: test, To: helperInTest, Kind: model.EdgeCalls})
	g.AddEdge(model.Edge{From: test, To: assertEq, Kind: model.EdgeCalls})

	rep := Analyze(g, nil)
	assert.Len(t, rep.Items, 0, "test helpers and test-support packages are not dead code")
	assert.Equal(t, rep.TestSupport, []string{"example.com/x/internal/testutil"})
	n, _ := g.Node(assertEq)
	assert.Equal(t, n.Liveness, TestSupport)
}
