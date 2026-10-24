package agentctx

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestChecklist(t *testing.T) {
	ctx := context.Background()
	const repo = "r"
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})
	node := func(kind model.NodeKind, local, file string, test bool) string {
		id := model.NodeID(repo, kind, local)
		g.AddNode(model.Node{ID: id, Kind: kind, Repo: repo, Name: local, QualifiedName: local, File: file, StartLine: 1, EndLine: 3, IsTest: test})
		return id
	}
	edge := func(from, to string, kind model.EdgeKind, ev model.Evidence) {
		g.AddEdge(model.Edge{From: from, To: to, Kind: kind, Evidence: ev})
	}
	iface := node(model.KindInterfaceMethod, "(app.I).Do", "app/i.go", false)
	concrete := node(model.KindMethod, "(*app.real).Do", "app/real.go", false)
	fake := node(model.KindMethod, "(*app.fakeI).Do", "app/real_test.go", false)
	edge(iface, concrete, model.EdgeDispatchesTo, "")
	edge(iface, fake, model.EdgeDispatchesTo, "")
	edge(node(model.KindFunction, "app.Direct", "app/use.go", false), concrete, model.EdgeCalls, model.EvidenceTyped)
	edge(node(model.KindFunction, "app.Use", "app/use.go", false), iface, model.EdgeCalls, model.EvidenceTyped)
	edge(node(model.KindFunction, "app.TestReal", "app/real_test.go", true), concrete, model.EdgeCalls, model.EvidenceTyped)
	edge(node(model.KindFunction, "app.Register", "app/routes.go", false), concrete, model.EdgeReferences, "")
	edge(node(model.KindSection, "README.md#usage", "README.md", false), concrete, model.EdgeMentions, "")
	edge(concrete, node(model.KindRecord, "docs/adr/0001.md", "docs/adr/0001.md", false), model.EdgeImplementsDecision, "")
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, repo, g))

	out, err := Service{R: mem}.Checklist(ctx, repo, "(*app.real).Do", ChangeSignature, 0, 0)
	require.NoError(t, err)
	got := map[string][]string{}
	for _, grp := range out.Groups {
		for _, it := range grp.Items {
			got[grp.Name] = append(got[grp.Name], it.QualifiedID)
		}
	}
	assert.Equal(t, got, map[string][]string{
		"interface":       {"(app.I).Do"},
		"implementations": {"(*app.fakeI).Do"},
		"callers":         {"app.Direct", "app.Use"},
		"tests":           {"app.TestReal"},
		"references":      {"app.Register"},
		"docs":            {"README.md#usage"},
		"decisions":       {"0001.md"},
	}, "callers of the interface method count: they break on a signature change too")
	assert.Equal(t, out.Total, 8)
	for _, grp := range out.Groups {
		switch grp.Name {
		case "implementations":
			assert.Contains(t, grp.Items[0].Why, "mock or fake", "the implementation in a test file is a double")
		case "callers":
			assert.Equal(t, grp.Items[0].Evidence, model.EvidenceTyped)
			assert.Contains(t, grp.Items[0].Why, "arguments and results")
		}
	}
	assert.True(t, out.Budget == nil)
	require.True(t, len(out.Notes) > 0, "says what the graph cannot see")

	_, err = Service{R: mem}.Checklist(ctx, repo, "(*app.real).Do", "move", 0, 0)
	assert.Error(t, err, "unknown change")
}
